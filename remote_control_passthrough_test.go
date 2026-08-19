package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRemoteControlRequestClassification(t *testing.T) {
	for _, value := range []string{
		"/codex/remote/control/enroll/start",
		"/remote-control-device-key",
		"browser-use-peer-authorization.node",
		"set-remote-control-connections-enabled",
		"remote_control_token=redacted",
	} {
		if !isRemoteControlRequestText(value) {
			t.Fatalf("remote-control marker was not recognized: %q", value)
		}
	}
	for _, value := range []string{"/v1/responses", "/v1/models", "https://api.example.test/v1"} {
		if isRemoteControlRequestText(value) {
			t.Fatalf("ordinary model request was misclassified as remote control: %q", value)
		}
	}
}

func TestRemoteControlRelayBypassDoesNotReachProviderInOfficialOrMixedMode(t *testing.T) {
	for _, mode := range []string{"official", "mixedApi"} {
		t.Run(mode, func(t *testing.T) {
			var providerCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				providerCalls.Add(1)
				w.WriteHeader(http.StatusTeapot)
			}))
			defer provider.Close()
			settings := defaultSettings()
			settings.RelayProfiles = []relayProfile{{
				ID:                "active",
				Name:              "Active",
				RelayMode:         mode,
				Protocol:          "responses",
				BaseURL:           provider.URL + "/v1",
				APIKey:            "provider-secret",
				OfficialMixAPIKey: mode == "mixedApi",
			}}
			settings.ActiveRelayID = "active"
			runtime := &launcherRuntime{settings: settings}

			for _, request := range []*http.Request{
				httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/remote-control/enroll/start", strings.NewReader(`{"device":"test"}`)),
				httptest.NewRequest(http.MethodPost, "http://127.0.0.1/codex/remote/control/device-key", strings.NewReader(`{"device":"test"}`)),
			} {
				recorder := httptest.NewRecorder()
				runtime.handleRelayProxyHTTP(recorder, request)
				if recorder.Code != http.StatusMisdirectedRequest {
					t.Fatalf("mode %s remote-control request status = %d, body=%s", mode, recorder.Code, recorder.Body.String())
				}
				body := recorder.Body.String()
				if !strings.Contains(body, `"official_remote_control_passthrough"`) {
					t.Fatalf("mode %s missing passthrough code: %s", mode, body)
				}
				if strings.Contains(body, provider.URL) || strings.Contains(body, "provider-secret") {
					t.Fatalf("mode %s leaked provider details: %s", mode, body)
				}
			}

			websocketRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/remote-control/websocket", nil)
			websocketRequest.Header.Set("Connection", "Upgrade")
			websocketRequest.Header.Set("Upgrade", "websocket")
			recorder := httptest.NewRecorder()
			runtime.handleRelayProxyHTTP(recorder, websocketRequest)
			if recorder.Code != http.StatusMisdirectedRequest || !strings.Contains(recorder.Body.String(), "official_remote_control_passthrough") {
				t.Fatalf("mode %s WebSocket remote-control request was not blocked: HTTP %d body=%s", mode, recorder.Code, recorder.Body.String())
			}
			if calls := providerCalls.Load(); calls != 0 {
				t.Fatalf("mode %s remote-control requests reached the model provider %d times", mode, calls)
			}
		})
	}
}

func TestRemoteControlInjectionKeepsNativeRequestsUnmodified(t *testing.T) {
	source := readFile("assets/inject/renderer-inject.js")
	for _, marker := range []string{
		"isCodexRemoteControlRequest",
		"remote_control_request_passthrough",
		"return params;",
		"return original(method, params, options);",
	} {
		if !strings.Contains(source, marker) {
			t.Fatalf("renderer injection missing remote-control passthrough marker %q", marker)
		}
	}
}
