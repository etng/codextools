package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKnownSessionIndexCandidateRequiresExactSchema(t *testing.T) {
	valid := []byte(`{"id":"thread-1","thread_name":"Task","updated_at":"2026-08-02T00:00:00Z"}`)
	candidate, ok := knownSessionIndexCandidate(valid)
	if !ok || candidate.ID != "thread-1" || candidate.ThreadName != "Task" {
		t.Fatalf("valid candidate = %#v, %v", candidate, ok)
	}
	invalid := [][]byte{
		[]byte(`{"id":"thread-1","thread_name":"Task"}`),
		[]byte(`{"id":"thread-1","thread_name":"Task","updated_at":"now","extra":true}`),
		[]byte(`{"id":"","thread_name":"Task","updated_at":"now"}`),
		[]byte(`{"id":"thread-1","thread_name":"Task","updated_at":1}`),
	}
	for _, line := range invalid {
		if _, ok := knownSessionIndexCandidate(line); ok {
			t.Fatalf("unexpected candidate for %s", line)
		}
	}
}

func TestPreviewSessionIndexCleanupProtectsRolloutAndSQLiteReferences(t *testing.T) {
	home := t.TempDir()
	filenameID := "018f2f15-b580-7000-8000-000000000001"
	metaID := "018f2f15-b580-7000-8000-000000000002"
	dbID := "018f2f15-b580-7000-8000-000000000003"
	orphanID := "018f2f15-b580-7000-8000-000000000004"
	rollout := filepath.Join(home, "sessions", "2026", "rollout-prefix-"+filenameID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"type":"session_meta","payload":{"id":"` + metaID + `"}}` + "\n"
	if err := os.WriteFile(rollout, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(home, "sqlite", "references.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE thread_spawn_edges (parent_thread_id TEXT, child_thread_id TEXT); INSERT INTO thread_spawn_edges VALUES (?, '')`, dbID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		sessionIndexLine(filenameID, "Filename live"),
		sessionIndexLine(metaID, "Metadata live"),
		sessionIndexLine(dbID, "SQLite live"),
		sessionIndexLine(orphanID, "Orphan"),
		`{"id":"unknown-shape","thread_name":"Do not touch","updated_at":"now","source":"cloud"}`,
	}
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(strings.Join(lines, "\r\n")+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := previewSessionIndexCleanup(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Candidates) != 1 || preview.Candidates[0].ID != orphanID {
		t.Fatalf("cleanup candidates = %#v", preview.Candidates)
	}
}

func TestApplySessionIndexCleanupBacksUpAndPreservesUnknownLines(t *testing.T) {
	home := t.TempDir()
	orphanID := "018f2f15-b580-7000-8000-000000000010"
	unknown := `{"id":"cloud-only","thread_name":"Cloud","updated_at":"now","source":"remote"}`
	original := sessionIndexLine(orphanID, "Orphan") + "\r\n" + unknown
	path := filepath.Join(home, "session_index.jsonl")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := previewSessionIndexCleanup(home)
	if err != nil {
		t.Fatal(err)
	}
	result, err := applySessionIndexCleanup(home, preview.SnapshotSHA256, []string{orphanID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.PrunedEntries != 1 || result.BackupDir == "" {
		t.Fatalf("cleanup result = %#v", result)
	}
	next, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(next) != unknown {
		t.Fatalf("unexpected filtered index: %q", next)
	}
	backup, err := os.ReadFile(filepath.Join(result.BackupDir, "session_index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != original {
		t.Fatalf("backup = %q, want %q", backup, original)
	}
	if !fileExists(filepath.Join(result.BackupDir, "metadata.json")) {
		t.Fatal("cleanup metadata backup is missing")
	}
}

func TestApplySessionIndexCleanupRejectsSnapshotAndLastMomentChanges(t *testing.T) {
	home := t.TempDir()
	id := "018f2f15-b580-7000-8000-000000000020"
	path := filepath.Join(home, "session_index.jsonl")
	if err := os.WriteFile(path, []byte(sessionIndexLine(id, "Orphan")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := previewSessionIndexCleanup(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applySessionIndexCleanup(home, "stale", []string{id}, false); err == nil || !strings.Contains(err.Error(), "预览后发生变化") {
		t.Fatalf("snapshot mismatch error = %v", err)
	}

	previous := sessionIndexCleanupBeforeWrite
	sessionIndexCleanupBeforeWrite = func() {
		_ = os.WriteFile(path, []byte(sessionIndexLine(id, "Changed")+"\n"), 0o600)
	}
	t.Cleanup(func() { sessionIndexCleanupBeforeWrite = previous })
	result, err := applySessionIndexCleanup(home, preview.SnapshotSHA256, []string{id}, false)
	if err == nil || !strings.Contains(err.Error(), "写入前再次发生变化") {
		t.Fatalf("last moment mutation result = %#v, error = %v", result, err)
	}
	var applyErr *sessionIndexCleanupApplyError
	if !errors.As(err, &applyErr) || applyErr.BackupDir == "" {
		t.Fatalf("mutation error should retain backup: %#v", err)
	}
}

func TestApplySessionIndexCleanupBlocksWhileCodexIsRunning(t *testing.T) {
	home := t.TempDir()
	id := "018f2f15-b580-7000-8000-000000000030"
	path := filepath.Join(home, "session_index.jsonl")
	if err := os.WriteFile(path, []byte(sessionIndexLine(id, "Orphan")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := previewSessionIndexCleanup(home)
	if err != nil {
		t.Fatal(err)
	}
	previous := detectProviderSyncActiveProcesses
	detectProviderSyncActiveProcesses = func() ([]string, error) { return []string{"ChatGPT"}, nil }
	t.Cleanup(func() { detectProviderSyncActiveProcesses = previous })
	if _, err := applySessionIndexCleanup(home, preview.SnapshotSHA256, []string{id}, true); err == nil || !strings.Contains(err.Error(), "完全退出") {
		t.Fatalf("active process should block cleanup: %v", err)
	}
	if fileExists(filepath.Join(home, "backups_state", "provider-sync")) {
		t.Fatal("blocked cleanup should not create a backup")
	}
}

func sessionIndexLine(id, title string) string {
	return `{"id":"` + id + `","thread_name":"` + title + `","updated_at":"2026-08-02T00:00:00Z"}`
}
