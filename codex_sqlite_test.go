package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func sqliteCountForTest(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCodexSessionDBPathsHonorSQLiteHomeAndMultipleDatabases(t *testing.T) {
	home := t.TempDir()
	sqliteHome := filepath.Join(t.TempDir(), "sqlite-home")
	t.Setenv("CODEX_SQLITE_HOME", sqliteHome)
	for _, path := range []string{
		filepath.Join(sqliteHome, "sqlite", "b.db"),
		filepath.Join(sqliteHome, "sqlite", "a.sqlite"),
		filepath.Join(sqliteHome, "state_5.sqlite"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		filepath.Join(sqliteHome, "sqlite", "a.sqlite"),
		filepath.Join(sqliteHome, "sqlite", "b.db"),
		filepath.Join(sqliteHome, "state_5.sqlite"),
	}
	if got := codexSessionDBPaths(home); !reflect.DeepEqual(got, want) {
		t.Fatalf("session DB paths = %#v, want %#v", got, want)
	}
	if got := codexLogsDBPath(home); got != filepath.Join(sqliteHome, "logs_2.sqlite") {
		t.Fatalf("logs DB = %q", got)
	}
}

func TestCodexSQLiteHomeKeepsMissingExplicitOverride(t *testing.T) {
	home := t.TempDir()
	override := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("CODEX_SQLITE_HOME", override)
	if got := codexSQLiteHome(home); got != override {
		t.Fatalf("missing explicit override should still win: %q", got)
	}
}

func TestAutomationRunRowsDeleteAndRestoreInOriginDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbPath := filepath.Join(home, ".codex", "sqlite", "automation.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := openSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE automation_runs (thread_id TEXT PRIMARY KEY, thread_title TEXT, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO automation_runs(thread_id, thread_title, status) VALUES (?, ?, ?)`, "auto-1", "Automation", "ready"); err != nil {
		t.Fatal(err)
	}
	rows, err := sqliteAutomationRunRowsByIDs(dbPath, []string{"auto-1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("automation rows = %#v, err = %v", rows, err)
	}
	lookup := sessionLookupResult{RequestedID: "auto-1", CanonicalID: "auto-1", Variants: []string{"auto-1"}, DBRows: rows}
	if err := deleteSessionLookupRows(lookup); err != nil {
		t.Fatal(err)
	}
	if got := sqliteCountForTest(t, db, `SELECT COUNT(*) FROM automation_runs WHERE thread_id = ?`, "auto-1"); got != 0 {
		t.Fatalf("automation row count after delete = %d", got)
	}
	if err := restoreSessionSQLiteRows(rows); err != nil {
		t.Fatal(err)
	}
	if got := sqliteCountForTest(t, db, `SELECT COUNT(*) FROM automation_runs WHERE thread_id = ?`, "auto-1"); got != 1 {
		t.Fatalf("automation row count after restore = %d", got)
	}
}

func TestAutomationBackupAllowsOnlyMatchingCodexRolloutPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	allowed := filepath.Join(home, ".codex", "sessions", "2026", "rollout-auto-1.jsonl")
	manifest := deletedSessionManifest{
		Version:   2,
		SessionID: "auto-1",
		Rows: []sessionSQLiteRow{{
			Table:  "automation_runs",
			Values: map[string]any{"thread_id": "auto-1"},
		}},
		Files: []deletedSessionFileBackup{{OriginalPath: allowed, BackupName: "files/rollout.jsonl"}},
	}
	if err := validateDeletedSessionRestorePaths(manifest); err != nil {
		t.Fatalf("matching automation rollout should be allowed: %v", err)
	}
	manifest.Files[0].OriginalPath = filepath.Join(t.TempDir(), "rollout-auto-1.jsonl")
	if err := validateDeletedSessionRestorePaths(manifest); err == nil {
		t.Fatal("automation backup must not restore outside Codex session roots")
	}
}

func TestProviderSyncDatabaseSnapshotsRestoreEveryDatabase(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first.db")
	second := filepath.Join(t.TempDir(), "second.db")
	if err := os.WriteFile(first, []byte("first-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := captureProviderSyncDatabaseSnapshots([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("first-after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second-after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreProviderSyncDatabaseSnapshots(snapshots); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{first: "first-before", second: "second-before"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("restored %s = %q, err = %v", path, data, err)
		}
	}
}
