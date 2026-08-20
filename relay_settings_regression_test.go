package main

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeSettingsMakesVisibleRelayBaseURLCanonical(t *testing.T) {
	settings := defaultSettings()
	settings.RelayProfiles = []relayProfile{{
		ID:              "relay",
		Name:            "Relay",
		BaseURL:         " https://new-relay.example.test/v1/ ",
		UpstreamBaseURL: "https://api.example.com/v1",
		APIKey:          "key",
		Protocol:        "responses",
		RelayMode:       "pureApi",
	}}
	settings.ActiveRelayID = "relay"

	normalized := normalizeSettings(settings)
	profile := activeRelayProfile(normalized)
	if got, want := profile.BaseURL, "https://new-relay.example.test/v1/"; got != want {
		t.Fatalf("visible base URL mismatch: got %q want %q", got, want)
	}
	if got, want := profile.UpstreamBaseURL, profile.BaseURL; got != want {
		t.Fatalf("stale upstream URL survived normalization: got %q want %q", got, want)
	}
	if got, want := effectiveUpstreamBaseURL(profile), profile.BaseURL; got != want {
		t.Fatalf("runtime upstream mismatch: got %q want %q", got, want)
	}
	endpoint, _ := relayTestPayload(profile, "gpt-test")
	if got, want := endpoint, "https://new-relay.example.test/v1/responses"; got != want {
		t.Fatalf("relay test endpoint mismatch: got %q want %q", got, want)
	}
}

func TestNormalizeSettingsMigratesLegacyUpstreamOnlyProfile(t *testing.T) {
	settings := defaultSettings()
	settings.RelayProfiles = []relayProfile{{
		ID:              "legacy",
		UpstreamBaseURL: "https://legacy-relay.example.test/v1",
		Protocol:        "responses",
		RelayMode:       "pureApi",
	}}

	profile := normalizeSettings(settings).RelayProfiles[0]
	if profile.BaseURL != profile.UpstreamBaseURL || profile.BaseURL != "https://legacy-relay.example.test/v1" {
		t.Fatalf("legacy upstream URL was not migrated: %#v", profile)
	}
}

func TestNormalizeSettingsRepairsUnknownActiveRelayID(t *testing.T) {
	settings := defaultSettings()
	settings.RelayProfiles = []relayProfile{
		{ID: "first", Protocol: "responses"},
		{ID: "second", Protocol: "responses"},
	}
	settings.ActiveRelayID = "removed-profile"

	normalized := normalizeSettings(settings)
	if got, want := normalized.ActiveRelayID, "first"; got != want {
		t.Fatalf("unknown active relay ID was not repaired: got %q want %q", got, want)
	}
}

func TestRelayRuntimeReloadsSavedSettingsOnWindowsAndMac(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	cached := defaultSettings()
	cached.RelayProfiles = []relayProfile{{
		ID:              "relay",
		Name:            "Relay",
		BaseURL:         "https://api.example.com/v1",
		UpstreamBaseURL: "https://api.example.com/v1",
		APIKey:          "old-key",
		Protocol:        "responses",
		RelayMode:       "pureApi",
	}}
	cached.ActiveRelayID = "relay"

	saved := cached
	saved.RelayProfiles = append([]relayProfile(nil), cached.RelayProfiles...)
	saved.RelayProfiles[0].BaseURL = "https://new-relay.example.test/v1"
	saved.RelayProfiles[0].APIKey = "new-key"
	saved.RelayProfiles[0].ImageGenerationEnabled = true
	saved.RelayProfiles[0].ImageGenerationUseSeparateAPI = true
	saved.RelayProfiles[0].ImageGenerationBaseURL = "https://images.example.test/v1"
	saved.RelayProfiles[0].ImageGenerationAPIKey = "image-key"
	saved.RelayProfiles[0].ProxyEnabled = true
	saved.RelayProfiles[0].ProxyURL = "http://127.0.0.1:7890"
	if err := saveSettings(saved); err != nil {
		t.Fatalf("save updated relay settings: %v", err)
	}

	originalGOOS := currentRuntimeGOOS
	t.Cleanup(func() { currentRuntimeGOOS = originalGOOS })
	runtimeState := &launcherRuntime{settings: cached}

	currentRuntimeGOOS = func() string { return "windows" }
	windowsProfile := activeRelayProfile(runtimeState.relaySettingsForRequest())
	if got, want := windowsProfile.BaseURL, "https://new-relay.example.test/v1"; got != want {
		t.Fatalf("Windows relay did not reload the saved base URL: got %q want %q", got, want)
	}
	if got, want := windowsProfile.APIKey, "new-key"; got != want {
		t.Fatalf("Windows relay did not reload the saved key: got %q want %q", got, want)
	}
	if got, want := windowsProfile.ImageGenerationBaseURL, "https://images.example.test/v1"; got != want {
		t.Fatalf("Windows relay did not reload the image base URL: got %q want %q", got, want)
	}
	if got, want := windowsProfile.ImageGenerationAPIKey, "image-key"; got != want {
		t.Fatalf("Windows relay did not reload the image key: got %q want %q", got, want)
	}
	if !windowsProfile.ProxyEnabled || windowsProfile.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("Windows relay did not reload the HTTP proxy: %#v", windowsProfile)
	}

	currentRuntimeGOOS = func() string { return "darwin" }
	macProfile := activeRelayProfile(runtimeState.relaySettingsForRequest())
	if got, want := macProfile.BaseURL, "https://new-relay.example.test/v1"; got != want {
		t.Fatalf("macOS relay did not reload the saved base URL: got %q want %q", got, want)
	}
	if got, want := macProfile.APIKey, "new-key"; got != want {
		t.Fatalf("macOS relay did not reload the saved key: got %q want %q", got, want)
	}
	if got, want := macProfile.ImageGenerationBaseURL, "https://images.example.test/v1"; got != want {
		t.Fatalf("macOS relay did not reload the image base URL: got %q want %q", got, want)
	}
	if got, want := macProfile.ImageGenerationAPIKey, "image-key"; got != want {
		t.Fatalf("macOS relay did not reload the image key: got %q want %q", got, want)
	}
	if !macProfile.ProxyEnabled || macProfile.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("macOS relay did not reload the HTTP proxy: %#v", macProfile)
	}
}

func TestRelayRuntimeSettingsReloadIsRateLimited(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	settings := defaultSettings()
	settings.ActiveRelayID = "relay"
	settings.RelayProfiles = []relayProfile{{ID: "relay", Model: "first-model", Protocol: "responses", RelayMode: "pureApi"}}
	if err := saveSettings(settings); err != nil {
		t.Fatalf("save first settings: %v", err)
	}
	runtimeState := &launcherRuntime{settings: defaultSettings()}
	if got := activeRelayProfile(runtimeState.relaySettingsForRequest()).Model; got != "first-model" {
		t.Fatalf("first runtime settings refresh got model %q", got)
	}

	settings.RelayProfiles[0].Model = "second-model"
	if err := saveSettings(settings); err != nil {
		t.Fatalf("save second settings: %v", err)
	}
	if got := activeRelayProfile(runtimeState.relaySettingsForRequest()).Model; got != "first-model" {
		t.Fatalf("runtime settings should stay cached inside refresh window, got model %q", got)
	}
	runtimeState.settingsRefreshMu.Lock()
	runtimeState.settingsRefreshedAt = time.Now().Add(-runtimeSettingsRefreshInterval)
	runtimeState.settingsRefreshMu.Unlock()
	if got := activeRelayProfile(runtimeState.relaySettingsForRequest()).Model; got != "second-model" {
		t.Fatalf("runtime settings should reload after refresh window, got model %q", got)
	}
}

func TestRelayEOFMessageExplainsUpstreamDisconnectAndFailover(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://secret-relay.example.test/v1/responses", Err: io.EOF}

	single := relayProxyRequestFailureMessage(err, 1)
	for _, expected := range []string{
		"upstream disconnected before returning an HTTP response (EOF)",
		"no failover candidate is configured",
	} {
		if !strings.Contains(single, expected) {
			t.Fatalf("single-candidate EOF message missing %q: %q", expected, single)
		}
	}
	if strings.Contains(single, "secret-relay.example.test") {
		t.Fatalf("user-facing relay error leaked the upstream host: %q", single)
	}

	multiple := relayProxyRequestFailureMessage(err, 2)
	if !strings.Contains(multiple, "all configured failover candidates failed") {
		t.Fatalf("multi-candidate EOF message did not explain failover exhaustion: %q", multiple)
	}
}

func TestRelayProfileSaveIsSeparateFromExplicitApply(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("web", "src", "App.tsx"))
	if err != nil {
		t.Fatalf("read App.tsx: %v", err)
	}
	source := string(data)
	start := strings.Index(source, "const saveDraft = async () =>")
	end := strings.Index(source[start:], "const switchDraft = () =>")
	if start < 0 || end < 0 {
		t.Fatal("relay profile save/apply functions were not found")
	}
	saveDraft := source[start : start+end]
	if strings.Contains(saveDraft, "switchRelayProfile") {
		t.Fatal("saving a relay profile still triggers a mode switch")
	}
	for _, expected := range []string{
		`if (!isSuccessStatus(settingsResult.status))`,
		`onSaved?.();`,
		`点击“重新应用”`,
		`upstreamBaseUrl: patch.baseUrl ?? ""`,
	} {
		if !strings.Contains(source, expected) {
			t.Fatalf("relay save regression guard missing %q", expected)
		}
	}
}

func TestInjectedBackendSettingSaveChecksResultBeforeCommitting(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("assets", "inject", "renderer-inject.js"))
	if err != nil {
		t.Fatalf("read renderer injection: %v", err)
	}
	source := string(data)
	for _, expected := range []string{
		`const previousSettings = codexPlusBackendSettings;`,
		`settings.status !== "ok"`,
		`codexPlusBackendSettings = previousSettings;`,
		`{ timeoutMs: 15000 }`,
		`backend_setting_save_failed`,
	} {
		if !strings.Contains(source, expected) {
			t.Fatalf("injected setting save guard missing %q", expected)
		}
	}
}
