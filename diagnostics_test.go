package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendDiagnosticLogRotatesOversizedFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := diagnosticLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(diagnosticLogMaxBytes); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	appendDiagnosticLog("rotation_test", map[string]any{"value": "ok"})

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) >= 4096 || !strings.Contains(string(current), `"event":"rotation_test"`) {
		t.Fatalf("rotated log should contain only the new record, size=%d content=%q", len(current), current)
	}
	backupInfo, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("rotated backup missing: %v", err)
	}
	if backupInfo.Size() != diagnosticLogMaxBytes {
		t.Fatalf("rotated backup size mismatch: got %d want %d", backupInfo.Size(), diagnosticLogMaxBytes)
	}
}

func TestTailFileReadsRecentLinesFromLargeSparseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 2); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteString("\none\ntwo\nthree"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	text, err := tailFile(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if text != "two\nthree" {
		t.Fatalf("unexpected log tail: %q", text)
	}
}

func TestAppendDiagnosticLogDiscardsRunawayLegacyFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := diagnosticLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(3 * diagnosticLogMaxBytes); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	appendDiagnosticLog("legacy_rotation_test", nil)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= 4096 {
		t.Fatalf("runaway legacy log was not discarded: %d bytes", info.Size())
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("runaway legacy log should not be retained as backup: %v", err)
	}
}

func TestRuntimeDiagnosticsReportsBoundedLogAndMemory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	appendDiagnosticLog("runtime_test", nil)
	value := runtimeDiagnosticsValue()
	if value["status"] != "ok" {
		t.Fatalf("unexpected runtime diagnostics status: %#v", value)
	}
	if value["diagnosticLogMaxBytes"] != diagnosticLogMaxBytes {
		t.Fatalf("diagnostic log limit missing: %#v", value)
	}
	if value["heapAllocBytes"] == uint64(0) || value["goroutines"] == 0 {
		t.Fatalf("runtime memory values missing: %#v", value)
	}
}
