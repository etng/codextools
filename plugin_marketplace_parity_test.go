package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarketplaceConfigAndInjectedSnapshotsMatchUpstreamStrategy(t *testing.T) {
	home := t.TempDir()
	curatedRoot := filepath.Join(home, ".tmp", "plugins")
	remoteRoot := filepath.Join(home, ".tmp", "plugins-remote")
	roleRoot := filepath.Join(home, ".tmp", "marketplaces", roleSpecificMarketplaceName)

	writeMarketplaceFixture(t, curatedRoot, "marketplace.json", `{"name":"openai-curated","plugins":[{"name":"gmail"}]}`)
	writeMarketplaceFixture(t, curatedRoot, "api_marketplace.json", `{"name":"openai-api-curated","plugins":[{"name":"build-web-apps"}]}`)
	writeMarketplaceFixture(t, remoteRoot, "marketplace.json", `{"name":"openai-curated-remote","plugins":[{"name":"product-design"}]}`)
	writeMarketplaceFixture(t, roleRoot, "marketplace.json", `{"name":"role-specific-plugins","plugins":[{"name":"sales"},{"name":"data-analytics"}]}`)
	writeTestFile(t, filepath.Join(curatedRoot, "plugins", "gmail", ".codex-plugin", "plugin.json"), `{"interface":{"displayName":"Gmail","composerIcon":"./icon.png"}}`)
	for _, fixture := range []struct{ root, name string }{{curatedRoot, "build-web-apps"}, {remoteRoot, "product-design"}, {roleRoot, "sales"}, {roleRoot, "data-analytics"}} {
		if err := os.MkdirAll(filepath.Join(fixture.root, "plugins", fixture.name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(home, "config.toml"), `[plugins."gmail@openai-curated"]
enabled = true

[plugins."sales@role-specific-plugins"]
enabled = false
`)

	changed, err := ensureOpenAICuratedMarketplaceConfig(home)
	if err != nil || !changed {
		t.Fatalf("ensure marketplace config: changed=%v err=%v", changed, err)
	}
	config := readFile(filepath.Join(home, "config.toml"))
	for _, table := range []string{"marketplaces.openai-curated", "marketplaces.openai-api-curated", "marketplaces.openai-curated-remote", "marketplaces.role-specific-plugins", `plugins."data-analytics@role-specific-plugins"`} {
		if !hasTable(config, table) {
			t.Fatalf("missing config table [%s]:\n%s", table, config)
		}
	}
	if tableValues(config, `plugins."sales@role-specific-plugins"`)["enabled"] != "false" {
		t.Fatalf("explicit disabled role plugin was overwritten:\n%s", config)
	}

	marketplaces := localPluginMarketplaces(home)
	if len(marketplaces) != 3 {
		t.Fatalf("marketplace snapshot count = %d, want 3: %#v", len(marketplaces), marketplaces)
	}
	curated := marketplaces[0].(map[string]any)
	gmail := curated["plugins"].([]any)[0].(map[string]any)
	if gmail["installed"] != true || gmail["marketplaceName"] != "openai-curated" || gmail["id"] != "gmail@openai-curated" {
		t.Fatalf("expanded plugin metadata mismatch: %#v", gmail)
	}
	pluginInterface := gmail["interface"].(map[string]any)
	wantIcon := filepath.Join(curatedRoot, "plugins", "gmail", "icon.png")
	if pluginInterface["composerIcon"] != wantIcon {
		t.Fatalf("absolute icon path = %q, want %q", pluginInterface["composerIcon"], wantIcon)
	}

	t.Setenv("CODEX_HOME", home)
	script := injectionScript(57321, defaultSettings())
	for _, marker := range []string{"window.__CODEX_PLUS_PLUGIN_MARKETPLACES__", `"openai-api-curated"`, "mergeLocalPluginMarketplaces", "plugin_marketplace_local_merged"} {
		if !strings.Contains(script, marker) {
			t.Fatalf("injection script missing %q", marker)
		}
	}
}

func TestMarketplaceConfigIsStableAfterFirstRepair(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".tmp", "plugins")
	writeMarketplaceFixture(t, root, "marketplace.json", `{"name":"openai-curated","plugins":[{"name":"gmail"}]}`)
	if err := os.MkdirAll(filepath.Join(root, "plugins", "gmail"), 0o755); err != nil {
		t.Fatal(err)
	}
	if changed, err := ensureOpenAICuratedMarketplaceConfig(home); err != nil || !changed {
		t.Fatalf("first repair: changed=%v err=%v", changed, err)
	}
	if changed, err := ensureOpenAICuratedMarketplaceConfig(home); err != nil || changed {
		t.Fatalf("second repair should be stable: changed=%v err=%v", changed, err)
	}
}

func TestEmbeddedRemoteMarketplaceInstallsAndRegisters(t *testing.T) {
	home := t.TempDir()
	if len(openAICuratedRemoteMarketplaceZip) < 1024 {
		t.Fatalf("embedded remote marketplace payload is unexpectedly small: %d", len(openAICuratedRemoteMarketplaceZip))
	}
	if err := installOpenAICuratedRemoteMarketplaceZip(home, openAICuratedRemoteMarketplaceZip); err != nil {
		t.Fatalf("install embedded marketplace: %v", err)
	}
	payload := currentRemotePluginMarketplacePayload(home)
	if payload.MarketplaceRoot == nil || payload.PluginCount == 0 || !payload.NeedsRepair {
		t.Fatalf("unexpected pre-registration status: %#v", payload)
	}
	if changed, err := ensureOpenAICuratedMarketplaceConfig(home); err != nil || !changed {
		t.Fatalf("register embedded marketplace: changed=%v err=%v", changed, err)
	}
	payload = currentRemotePluginMarketplacePayload(home)
	if !payload.ConfigRegistered || payload.NeedsRepair || payload.PluginCount == 0 {
		t.Fatalf("unexpected registered status: %#v", payload)
	}
}

func TestPluginMarketplaceZipPathRejectsTraversal(t *testing.T) {
	for _, path := range []string{"../escape", "/absolute", "plugins/../../escape", "plugins//escape", ""} {
		if _, err := safePluginMarketplaceZipPath(path); err == nil {
			t.Fatalf("unsafe zip path accepted: %q", path)
		}
	}
	if got, err := safePluginMarketplaceZipPath("plugins/example/file.txt"); err != nil || got != filepath.Join("plugins", "example", "file.txt") {
		t.Fatalf("safe zip path = %q, %v", got, err)
	}
}

func writeMarketplaceFixture(t *testing.T, root, filename, contents string) {
	t.Helper()
	writeTestFile(t, filepath.Join(root, ".agents", "plugins", filename), contents)
}
