package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderSyncMetadataReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-large.jsonl")
	firstLine := `{"type":"session_meta","payload":{"id":"thread-large","cwd":"/project","model_provider":"openai"}}` + "\n"
	if err := os.WriteFile(path, []byte(firstLine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(providerSyncMetadataReadLimit*8)); err != nil {
		t.Fatal(err)
	}
	data, truncated, err := readProviderSyncMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(data) > providerSyncMetadataReadLimit {
		t.Fatalf("metadata read was not bounded: truncated=%v bytes=%d", truncated, len(data))
	}
	if !strings.Contains(string(data), `"model_provider":"openai"`) {
		t.Fatalf("session metadata was not retained: %q", data)
	}
}

func TestTargetedRemoteControlRecoveryUsesOneRollout(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	distractor := filepath.Join(sessions, "rollout-0000-distractor.jsonl")
	if err := os.WriteFile(distractor, []byte(`{"type":"session_meta","payload":{"id":"other-thread","model_provider":"openai"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(distractor, 32*1024*1024); err != nil {
		t.Fatal(err)
	}
	threadID := "0198f7f0-1111-7777-8000-111111111111"
	target := filepath.Join(sessions, "rollout-"+threadID+".jsonl")
	if err := os.WriteFile(target, []byte(`{"type":"session_meta","payload":{"id":"`+threadID+`","cwd":"/project","model_provider":"openai"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	change, found, err := targetedRemoteControlSessionChange(home, threadID, "vendor_remote")
	if err != nil || !found {
		t.Fatalf("targeted recovery lookup = found %v, err %v", found, err)
	}
	if change.Path != target || !change.RewriteNeeded || change.ThreadID != threadID {
		t.Fatalf("unexpected targeted change: %#v", change)
	}
}

func TestRemoteControlRecoveryCoalescesDuplicateRequests(t *testing.T) {
	key := t.Name()
	if !beginRemoteControlRecovery(key) {
		t.Fatal("first recovery request was not accepted")
	}
	if beginRemoteControlRecovery(key) {
		t.Fatal("duplicate recovery request was not coalesced")
	}
	finishRemoteControlRecovery(key)
	if !beginRemoteControlRecovery(key) {
		t.Fatal("recovery key remained locked after completion")
	}
	finishRemoteControlRecovery(key)
}

func TestRemoteControlRecoveryRetryStopsWhileInProgress(t *testing.T) {
	source := readFile(filepath.Join("assets", "inject", "renderer-inject.js"))
	if !strings.Contains(source, `result?.status === "in_progress"`) {
		t.Fatal("renderer retries recovery while an earlier request is still scanning")
	}
}
