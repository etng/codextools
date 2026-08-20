package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestZedRemoteBuildsEncodedSSHURL(t *testing.T) {
	user, host, port, err := splitSSHAuthority("dev@[2001:db8::1]:2200")
	if err != nil {
		t.Fatalf("split SSH authority failed: %v", err)
	}
	if user != "dev" || host != "[2001:db8::1]" || port == nil || *port != 2200 {
		t.Fatalf("unexpected SSH authority parse: user=%q host=%q port=%v", user, host, port)
	}

	url, err := buildZedRemoteURL(sshTarget{User: user, Host: host, Port: port}, "/workspace/hello world/a#b.go")
	if err != nil {
		t.Fatalf("build Zed URL failed: %v", err)
	}
	if want := "ssh://dev@[2001:db8::1]:2200/workspace/hello%20world/a%23b.go"; url != want {
		t.Fatalf("Zed URL mismatch:\n got: %s\nwant: %s", url, want)
	}
}

func TestZedRemoteFallbackUsesGlobalStateThreadHints(t *testing.T) {
	state := map[string]any{
		"selected-remote-host-id": "host-1",
		"codex-managed-remote-connections": []any{
			map[string]any{"hostId": "host-1", "sshHost": "dev@example.com:2222"},
		},
		"thread-workspace-root-hints": map[string]any{
			"local:thread-1": map[string]any{"hostId": "host-1", "remotePath": "/work/project/sub"},
		},
		"remote-projects": []any{
			map[string]any{"id": "project-1", "hostId": "host-1", "remotePath": "/work/project"},
		},
	}

	request, err := fallbackOpenRequestFromGlobalStateWithContext(state, "", "thread-1", "", "")
	if err != nil {
		t.Fatalf("fallback request failed: %v", err)
	}
	ssh, _ := request["ssh"].(map[string]any)
	if stringFromAny(request["hostId"]) != "host-1" || stringFromAny(request["path"]) != "/work/project/sub" {
		t.Fatalf("unexpected fallback request: %#v", request)
	}
	if stringFromAny(ssh["user"]) != "dev" || stringFromAny(ssh["host"]) != "example.com" {
		t.Fatalf("unexpected fallback ssh target: %#v", ssh)
	}
	port, _ := ssh["port"].(*uint16)
	if port == nil || *port != 2222 {
		t.Fatalf("unexpected fallback ssh port: %#v", ssh["port"])
	}
}

func TestUpstreamWorktreeParsers(t *testing.T) {
	if got := defaultRemoteName([]string{"origin", "upstream"}); got != "upstream" {
		t.Fatalf("default remote should prefer upstream, got %q", got)
	}
	if err := validateRemoteName("../origin"); err == nil {
		t.Fatal("remote validation should reject path-like names")
	}

	refs := refsFromOutput(strings.Join([]string{
		"refs/remotes/origin/HEAD",
		"refs/remotes/origin/main",
		"refs/remotes/origin/feature/x",
		"refs/remotes/upstream/ignored",
	}, "\n"), "origin", "main")
	labels := []string{stringFromAny(refs[0]["label"]), stringFromAny(refs[1]["label"])}
	if !reflect.DeepEqual(labels, []string{"origin/feature/x", "origin/main"}) {
		t.Fatalf("unexpected refs: %#v", refs)
	}

	root := t.TempDir()
	other := filepath.Join(t.TempDir(), "feature")
	branches := worktreeBranchesFromOutput("worktree " + root + "\nbranch refs/heads/main\n\nworktree " + other + "\nbranch refs/heads/feature/x\n\nworktree /detached\nHEAD abc\n\n")
	if len(branches) != 2 {
		t.Fatalf("unexpected worktree branch count: %#v", branches)
	}
	if stringFromAny(branches[0]["branch"]) != "main" || stringFromAny(branches[1]["branch"]) != "feature/x" {
		t.Fatalf("unexpected worktree branches: %#v", branches)
	}
}

func TestContextConfigSelectionFiltersDisabledAndUnselected(t *testing.T) {
	config := strings.Join([]string{
		`model = "gpt-5"`,
		`[model_providers.local]`,
		`base_url = "https://example.test/v1"`,
		`[mcp_servers.context7]`,
		`command = "npx"`,
		`[skills.writer]`,
		`enabled = false`,
		`path = "/skills/writer"`,
		`[plugins."github@openai-curated"]`,
		`enabled = true`,
	}, "\n")

	common, contextConfig := splitContextConfigSections(config)
	if strings.Contains(common, "[mcp_servers.context7]") || !strings.Contains(contextConfig, "[mcp_servers.context7]") {
		t.Fatalf("context split mismatch:\ncommon:\n%s\ncontext:\n%s", common, contextConfig)
	}

	filtered := filterCommonConfigForSelection(config, relayContextSelection{
		MCPServers: []string{"context7"},
		Plugins:    []string{"github@openai-curated"},
	})
	for _, expected := range []string{`model = "gpt-5"`, `[model_providers.local]`, `[mcp_servers.context7]`, `[plugins."github@openai-curated"]`} {
		if !strings.Contains(filtered, expected) {
			t.Fatalf("filtered config missing %q:\n%s", expected, filtered)
		}
	}
	if strings.Contains(filtered, "[skills.writer]") {
		t.Fatalf("disabled or unselected skill should be filtered:\n%s", filtered)
	}

	emptySelection := filterCommonConfigForSelection(config, relayContextSelection{})
	for _, forbidden := range []string{"[mcp_servers.context7]", "[skills.writer]", `[plugins."github@openai-curated"]`} {
		if strings.Contains(emptySelection, forbidden) {
			t.Fatalf("empty selection should omit context table %q:\n%s", forbidden, emptySelection)
		}
	}
}

func TestModelCatalogParsesConfiguredSources(t *testing.T) {
	models := parseModelPayload(map[string]any{
		"data": []any{
			map[string]any{"id": "gpt-5"},
			map[string]any{"name": "custom-model"},
			"inline-model",
		},
	})
	if !reflect.DeepEqual(models, []string{"gpt-5", "custom-model", "inline-model"}) {
		t.Fatalf("unexpected parsed models: %#v", models)
	}
	if got := modelsEndpoint("https://api.example.test/openai/v1"); got != "https://api.example.test/openai/v1/models" {
		t.Fatalf("models endpoint mismatch: %s", got)
	}
	if got := safeStatusURL("https://user:secret@example.test/v1?token=hidden#frag"); got != "https://example.test/v1" {
		t.Fatalf("safe URL should strip credentials/query/fragment, got %s", got)
	}
}

func TestBridgeParityRoutesExistAndAdsStayAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runtime := &launcherRuntime{}
	for _, route := range []string{
		"/user-scripts/delete",
		"/zed-remote/status",
		"/zed-remote/resolve-host",
		"/zed-remote/fallback-request",
		"/upstream-worktree/status",
		"/upstream-worktree/defaults",
		"/upstream-worktree/prepare",
		"/upstream-worktree/create",
	} {
		result := runtime.handleBridgeRequest(route, json.RawMessage(`{}`))
		if stringFromAny(result["message"]) == "Unknown bridge path" || stringFromAny(result["path"]) == route {
			t.Fatalf("bridge route %s was not registered: %#v", route, result)
		}
	}
	ads := runtime.handleBridgeRequest("/ads", json.RawMessage(`{}`))
	if stringFromAny(ads["message"]) != "Unknown bridge path" {
		t.Fatalf("/ads should remain unimplemented, got %#v", ads)
	}
}

func TestHighFrequencyBridgeDiagnosticsAreSuppressed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runtime := &launcherRuntime{}

	runtime.handleBridgeRequest("/settings/get", json.RawMessage(`{}`))
	runtime.handleBridgeRequest("/diagnostics/log", json.RawMessage(`{"event":"performance_test","detail":{}}`))
	logs := readFile(diagnosticLogPath())
	if strings.Contains(logs, `"event":"bridge.request"`) || strings.Contains(logs, `"event":"bridge.response"`) {
		t.Fatalf("high-frequency bridge routes should not emit request/response trace logs: %s", logs)
	}
	if !strings.Contains(logs, `"event":"renderer.performance_test"`) {
		t.Fatalf("renderer diagnostic should still be persisted: %s", logs)
	}
	for _, path := range []string{"/settings/get", "/thread-sort-keys", "/diagnostics/log", "/diagnostics/runtime", "/v1/responses", "/v1/live"} {
		if shouldTraceHelperRequest(path) {
			t.Fatalf("helper request path %s should be quiet", path)
		}
	}
	if !shouldTraceHelperRequest("/delete") {
		t.Fatal("session delete should retain a diagnostic trace")
	}
}

func TestBackendHeartbeatUsesRuntimeSettingsSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cached := defaultSettings()
	cached.ActiveRelayID = "cached"
	cached.RelayProfiles = []relayProfile{{ID: "cached", HideOfficialUsageAlert: true}}
	onDisk := cached
	onDisk.RelayProfiles = []relayProfile{{ID: "cached", HideOfficialUsageAlert: false}}
	if err := saveSettings(onDisk); err != nil {
		t.Fatalf("save disk settings: %v", err)
	}

	result := (&launcherRuntime{settings: cached}).handleBridgeRequest("/backend/status", json.RawMessage(`{}`))
	if !boolFromAny(result["hideOfficialUsageAlert"]) {
		t.Fatalf("backend heartbeat should use runtime settings without rereading disk: %#v", result)
	}
}

func TestInjectionPerformanceGuardsArePresent(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("assets", "inject", "renderer-inject.js"))
	if err != nil {
		t.Fatalf("read renderer injection failed: %v", err)
	}
	source := string(script)
	for _, marker := range []string{
		"codexAppAssetCandidateUrlsCacheTtlMs = 1000",
		"appServerRequestCandidatesCacheTtlMs = 60000",
		"codexModelCatalogCacheTtlMs = 60000",
		"pluginMarketplaceRequestPatchRetryDelaysMs = [500, 1500, 3000, 6000, 12000, 30000]",
		"appServerModelRequestPatchRetryDelaysMs = [250, 500, 1000, 2000, 4000, 8000, 15000]",
		"appServerModelRequestPatchMissCount >= appServerModelRequestPatchMaxMisses",
		"typeof window.__codexSessionDeleteBridge === \"function\"",
		"chatsSortRefreshIntervalMs = 5000",
		"chatsSortDbRefreshIntervalMs = 20000",
		"codexPlusBackendHeartbeatVisibleIntervalMs = 15000",
		"codexPlusBackendHeartbeatHiddenIntervalMs = 60000",
		"scheduleConversationViewAlign(1), 2000",
		"conversationViewReleaseElement",
		"rememberCodexRuntimeFailure",
	} {
		if !strings.Contains(source, marker) {
			t.Fatalf("renderer injection missing performance guard %q", marker)
		}
	}
}

func TestBridgeSettingsIncludesRuntimeCodexAppVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runtime := &launcherRuntime{codexAppPath: filepath.Join("C:", "Program Files", "WindowsApps", "OpenAI.ChatGPT_26.601.2237.0_x64__2p2nqsd0c76g0", "app")}

	result := runtime.handleBridgeRequest("/settings/get", json.RawMessage(`{}`))

	if got := stringFromAny(result["codexAppVersion"]); got != "26.601.2237.0" {
		t.Fatalf("bridge settings should expose runtime Codex app version, got %#v from %#v", got, result)
	}
}

func TestInjectionScriptInjectsLocalPluginMarketplacesWithoutAds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	manifest := filepath.Join(home, ".codex", ".tmp", "plugins", ".agents", "plugins", "marketplace.json")
	writeTestFile(t, manifest, `{"name":"openai-curated","plugins":[{"name":"writer"}]}`)
	script := injectionScript(57321, defaultSettings())

	for _, expected := range []string{"window.__CODEX_PLUS_PLUGIN_MARKETPLACES__", `"writer"`, `"openai-curated"`} {
		if !strings.Contains(script, expected) {
			t.Fatalf("injection should include normalized local marketplace payload %q", expected)
		}
	}
	if strings.Contains(script, "/ads") {
		t.Fatal("local marketplace payload must not reintroduce ads")
	}
}

func TestInjectionScriptIncludesV1224RuntimeConfig(t *testing.T) {
	settings := defaultSettings()
	settings.CodexAppFastStartup = true
	settings.CodexAppForceChineseLocale = true
	settings.CodexAppNativeMenuLocalization = true

	script := injectionScript(57321, settings)

	for _, expected := range []string{
		"__CODEX_PLUS_FAST_STARTUP__",
		"__CODEX_PLUS_FORCE_CHINESE_LOCALE__",
		"__CODEX_PLUS_NATIVE_MENU_LOCALIZATION__",
		"codexAppPasteFix",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("injection script missing v1.2.24 marker %q", expected)
		}
	}
}

func TestImageOverlayConfigAndInjectionScript(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "overlay.png")
	if err := os.WriteFile(imagePath, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatalf("write overlay image failed: %v", err)
	}
	settings := defaultSettings()
	settings.CodexAppImageOverlayEnabled = true
	settings.CodexAppImageOverlayPath = imagePath
	settings.CodexAppImageOverlayOpacity = 42
	settings.CodexAppImageOverlayFitMode = "tile"

	config := imageOverlayConfig(57321, settings)

	if !boolFromAny(config["enabled"]) {
		t.Fatalf("overlay should be enabled: %#v", config)
	}
	if got := stringFromAny(config["dataUrl"]); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("overlay data URL mismatch: %q", got)
	}
	if got := stringFromAny(config["imageUrl"]); got != "http://127.0.0.1:57321/overlay/image" {
		t.Fatalf("overlay image URL mismatch: %q", got)
	}
	if got := stringFromAny(config["fitMode"]); got != "tile" {
		t.Fatalf("overlay fit mode mismatch: %q", got)
	}
	script := injectionScript(57321, settings)
	for _, expected := range []string{"__CODEX_PLUS_IMAGE_OVERLAY__", "codex-plus-image-overlay", "image_overlay_installed"} {
		if !strings.Contains(script, expected) {
			t.Fatalf("injection script missing overlay marker %q", expected)
		}
	}
}

func TestBridgeSettingsPersistLatestEnhancementFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runtime := &launcherRuntime{}
	result := runtime.handleBridgeRequest("/settings/set", json.RawMessage(`{
  "codexAppPluginMarketplaceUnlock": false,
  "codexAppPluginAutoExpand": false,
  "codexAppFastStartup": true,
  "codexAppImageOverlayFitMode": "stretch"
}`))
	if stringFromAny(result["status"]) != "ok" {
		t.Fatalf("settings set failed: %#v", result)
	}
	loaded := loadSettings()
	if loaded.CodexAppPluginMarketplaceUnlock || loaded.CodexAppPluginAutoExpand {
		t.Fatalf("explicitly disabled plugin settings were not preserved: %#v", loaded)
	}
	if !loaded.CodexAppFastStartup || loaded.CodexAppImageOverlayFitMode != "stretch" {
		t.Fatalf("latest enhancement fields were not persisted: %#v", loaded)
	}
}

func TestResilientLoopbackGuardHoldsLockAndListener(t *testing.T) {
	port := freeLoopbackPort(t)
	guard, err := acquireResilientLoopbackPortGuardAt(port, t.TempDir())
	if err != nil {
		t.Fatalf("guard acquisition failed: %v", err)
	}
	defer guard.release()

	if guard.listener == nil {
		t.Fatal("guard should hold loopback listener when port is available")
	}
	if guard.fallbackPath() != "" {
		t.Fatalf("available port should not use fallback lock: %s", guard.fallbackPath())
	}
}

func TestResilientLoopbackGuardReportsLockConflict(t *testing.T) {
	port := freeLoopbackPort(t)
	root := t.TempDir()
	guard, err := acquireResilientLoopbackPortGuardAt(port, root)
	if err != nil {
		t.Fatalf("first guard acquisition failed: %v", err)
	}
	defer guard.release()

	second, err := acquireResilientLoopbackPortGuardAt(port, root)
	if err == nil {
		if second != nil {
			second.release()
		}
		t.Fatal("second guard acquisition should fail while lock is held")
	}
	if !isLoopbackGuardBusyError(err) {
		t.Fatalf("expected lock busy error, got %T %v", err, err)
	}
}

func TestResilientLoopbackGuardReportsConnectablePortConflict(t *testing.T) {
	errBusy := fmt.Errorf("listen tcp 127.0.0.1:57320: %w", syscall.EADDRINUSE)
	guard, err := acquireResilientLoopbackPortGuardWith(
		57320,
		t.TempDir(),
		func(uint16) (net.Listener, error) { return nil, errBusy },
		func(uint16) bool { return true },
	)
	if guard != nil {
		guard.release()
	}
	if err == nil || !isAddrInUseError(err) {
		t.Fatalf("connectable busy port should return addr-in-use error, got %T %v", err, err)
	}
}

func TestResilientLoopbackGuardUsesFallbackForStalePort(t *testing.T) {
	errBusy := fmt.Errorf("listen tcp 127.0.0.1:57320: %w", syscall.EADDRINUSE)
	root := t.TempDir()
	guard, err := acquireResilientLoopbackPortGuardWith(
		57320,
		root,
		func(uint16) (net.Listener, error) { return nil, errBusy },
		func(uint16) bool { return false },
	)
	if err != nil {
		t.Fatalf("stale busy port should use fallback lock: %v", err)
	}
	defer guard.release()
	if guard.listener != nil {
		t.Fatal("fallback guard should not hold a listener")
	}
	if guard.fallbackPath() == "" {
		t.Fatal("fallback guard should expose fallback lock path")
	}

	second, err := acquireResilientLoopbackPortGuardWith(
		57320,
		root,
		func(uint16) (net.Listener, error) { return nil, errBusy },
		func(uint16) bool { return false },
	)
	if err == nil {
		if second != nil {
			second.release()
		}
		t.Fatal("second fallback guard should fail while lock is held")
	}
	if !isLoopbackGuardBusyError(err) {
		t.Fatalf("expected fallback lock busy error, got %T %v", err, err)
	}
}

func freeLoopbackPort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate loopback port: %v", err)
	}
	defer listener.Close()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to split loopback address: %v", err)
	}
	var port uint16
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
		t.Fatalf("failed to parse loopback port %q: %v", portText, err)
	}
	return port
}
