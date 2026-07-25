package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUserScriptInventoryMergesValidatedRuntimeStatus(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(userScriptsDir(), "runtime.js")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("window.runtimeTest = true;"), 0o644); err != nil {
		t.Fatal(err)
	}

	inventory := scanUserScripts(map[string]any{
		"user:runtime.js": map[string]any{"status": "failed", "error": "boom"},
		"user:missing.js": map[string]any{"status": "loaded"},
	})
	if len(inventory.Scripts) != 1 {
		t.Fatalf("scripts = %#v", inventory.Scripts)
	}
	item := inventory.Scripts[0]
	if item.Status != "failed" || item.Error != "boom" {
		t.Fatalf("runtime status was not merged: %#v", item)
	}

	inventory = scanUserScripts(map[string]any{
		"scripts": map[string]any{"user:runtime.js": map[string]any{"status": "arbitrary", "error": strings.Repeat("x", 5000)}},
	})
	item = inventory.Scripts[0]
	if item.Status != "not_loaded" || len([]rune(item.Error)) != 4000 {
		t.Fatalf("runtime status should be normalized and bounded: %#v", item)
	}
}

func TestEnabledUserScriptBundleRecordsRuntimeLifecycle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(userScriptsDir(), "lifecycle.js")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("window.lifecycleTest = true;"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := enabledUserScriptBundle()
	for _, expected := range []string{"__codexPlusUserScripts", `status = "loaded"`, `status = "failed"`, `user:lifecycle.js`} {
		if !strings.Contains(bundle, expected) {
			t.Fatalf("bundle missing %q:\n%s", expected, bundle)
		}
	}
}

func TestUserScriptListBridgeAcceptsRendererRuntimeStatus(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(userScriptsDir(), "bridge.js")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("window.bridgeTest = true;"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := (&launcherRuntime{}).handleBridgeRequest("/user-scripts/list", json.RawMessage(`{
  "runtime_status": {"user:bridge.js": {"status": "loaded", "error": ""}}
}`))
	items, ok := result["scripts"].([]userScriptInventoryItem)
	if !ok || len(items) != 1 || items[0].Status != "loaded" {
		t.Fatalf("bridge runtime status was not merged: %#v", result)
	}
}
