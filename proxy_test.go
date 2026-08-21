package main

import (
	"strings"
	"testing"
)

func TestGlobalProxyDefaultsAreDisabledAndKeepLoopbackBypass(t *testing.T) {
	settings := defaultSettings()
	if settings.ProxyEnabled || settings.ProxyRelayEnabled || settings.ProxyRemoteControlEnabled || settings.ProxyOfficialAuthEnabled {
		t.Fatal("global proxy should be disabled by default")
	}
	if settings.ProxyNoProxy != defaultProxyNoProxy {
		t.Fatalf("default no-proxy list = %q, want %q", settings.ProxyNoProxy, defaultProxyNoProxy)
	}
}

func TestConfiguredProxyURLValidation(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:10809", "https://user:secret@example.test:8443"} {
		if parsed, err := parseConfiguredProxyURL(raw); err != nil || parsed == nil {
			t.Fatalf("proxy URL %q should be accepted: %v", raw, err)
		}
	}
	for _, raw := range []string{"", "socks5://127.0.0.1:1080", "ftp://example.test", "http:///missing-host"} {
		if raw == "" {
			continue
		}
		if _, err := parseConfiguredProxyURL(raw); err == nil {
			t.Fatalf("proxy URL %q should be rejected", raw)
		}
	}
}

func TestEffectiveProxyURLProfileTakesPrecedenceForRelay(t *testing.T) {
	settings := defaultSettings()
	settings.ProxyEnabled = true
	settings.ProxyRelayEnabled = true
	settings.ProxyURL = "http://global.example:8080"
	profile := relayProfile{ProxyEnabled: true, ProxyURL: "http://profile.example:8081"}
	got, err := effectiveProxyURL(settings, profile, proxyPurposeRelay)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Host != "profile.example:8081" {
		t.Fatalf("relay profile proxy should win, got %#v", got)
	}
	profile.ProxyEnabled = false
	got, err = effectiveProxyURL(settings, profile, proxyPurposeRelay)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Host != "global.example:8080" {
		t.Fatalf("global relay proxy should be used as fallback, got %#v", got)
	}
}

func TestNativeProxyEnvironmentAndChromiumArguments(t *testing.T) {
	settings := defaultSettings()
	settings.ProxyEnabled = true
	settings.ProxyURL = "http://127.0.0.1:10809"
	settings.ProxyRemoteControlEnabled = true
	env := proxyEnvironmentForSettings(settings)
	joined := strings.Join(env, "\n")
	for _, key := range []string{"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=", "http_proxy=", "https_proxy=", "all_proxy=", "NO_PROXY=", "no_proxy="} {
		if !strings.Contains(joined, key) {
			t.Fatalf("proxy environment missing %s: %s", key, joined)
		}
	}
	args := buildCodexArgumentsForSettings(9229, nil, settings)
	if !containsString(args, "--proxy-server=http://127.0.0.1:10809") {
		t.Fatalf("remote-control launch should include proxy-server: %#v", args)
	}
	if !containsString(args, "--proxy-bypass-list=127.0.0.1;localhost;[::1]") {
		t.Fatalf("remote-control launch should preserve loopback bypass: %#v", args)
	}
	settings.ProxyRemoteControlEnabled = false
	args = buildCodexArgumentsForSettings(9229, nil, settings)
	if containsString(args, "--proxy-server=http://127.0.0.1:10809") {
		t.Fatalf("disabled remote-control proxy should not alter launch args: %#v", args)
	}
}

func TestAdvancedVoiceProxyUsesChromiumAndWebRTCProxyPath(t *testing.T) {
	settings := defaultSettings()
	settings.ProxyEnabled = true
	settings.ProxyURL = "http://127.0.0.1:10809"
	settings.ProxyRealtimeEnabled = true

	args := buildCodexArgumentsForSettings(9229, nil, settings)
	for _, expected := range []string{
		"--proxy-server=http://127.0.0.1:10809",
		"--proxy-bypass-list=127.0.0.1;localhost;[::1]",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
	} {
		if !containsString(args, expected) {
			t.Fatalf("advanced voice launch is missing %q: %#v", expected, args)
		}
	}
	if !nativeDesktopProxyEnabled(settings) {
		t.Fatal("realtime proxy should enable native desktop proxy")
	}
	if len(proxyEnvironmentForSettings(settings)) == 0 {
		t.Fatal("realtime proxy should inject native desktop proxy environment")
	}
}

func TestRealtimeGlobalProxyIsIndependentFromRelayBaseURL(t *testing.T) {
	settings := defaultSettings()
	settings.ProxyEnabled = true
	settings.ProxyRealtimeEnabled = true
	settings.ProxyURL = "http://127.0.0.1:10809"
	profile := relayProfile{BaseURL: "https://third-party.example/v1", ProxyEnabled: true, ProxyURL: "http://profile.example:8081"}
	got, err := effectiveProxyURL(settings, profile, proxyPurposeRealtime)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Host != "127.0.0.1:10809" {
		t.Fatalf("realtime should prefer global official proxy, got %#v", got)
	}
}
