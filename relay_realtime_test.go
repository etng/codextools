package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func realtimeTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT claims failed: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func realtimeTestAuthJSON(t *testing.T, accountID, email string, expiresAt time.Time) string {
	t.Helper()
	idToken := realtimeTestJWT(t, map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
	accessToken := realtimeTestJWT(t, map[string]any{
		"exp": expiresAt.Unix(),
		"sub": "realtime-test-user",
	})
	data, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      idToken,
			"access_token":  accessToken,
			"refresh_token": "refresh-token",
			"account_id":    accountID,
		},
	})
	if err != nil {
		t.Fatalf("marshal auth JSON failed: %v", err)
	}
	return string(data)
}

func realtimeTestRuntime(t *testing.T, authContents string, thirdPartyBaseURL string) (*launcherRuntime, backendSettings, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("create CODEX_HOME failed: %v", err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	profile := relayProfile{
		ID:                   "active",
		Name:                 "Third-party text relay",
		RelayMode:            "mixedApi",
		Protocol:             "responses",
		BaseURL:              thirdPartyBaseURL,
		APIKey:               "third-party-text-key",
		OfficialAuthContents: authContents,
		AuthContents:         authContents,
	}
	settings := defaultSettings()
	settings.RelayProfiles = []relayProfile{profile}
	settings.ActiveRelayID = profile.ID
	if err := saveSettings(settings); err != nil {
		t.Fatalf("save settings failed: %v", err)
	}
	runtime := &launcherRuntime{settings: settings}
	return runtime, settings, codexHome
}

func TestOfficialRealtimeCapabilityPrefersMatchingCurrentAuth(t *testing.T) {
	now := time.Now()
	bound := realtimeTestAuthJSON(t, "account-1", "bound@example.com", now.Add(-time.Hour))
	runtime, settings, codexHome := realtimeTestRuntime(t, bound, "https://relay.invalid/v1")
	current := realtimeTestAuthJSON(t, "account-1", "current@example.com", now.Add(time.Hour))
	writeTestFile(t, filepath.Join(codexHome, "auth.json"), current)

	capability := runtime.officialRealtimeCapability(settings)
	if !capability.Available || capability.Reason != officialRealtimeAvailableReason {
		t.Fatalf("matching current auth should enable realtime: %#v", capability)
	}
	if capability.Credentials.Source != filepath.Join(codexHome, "auth.json") {
		t.Fatalf("current auth should be preferred, source=%q", capability.Credentials.Source)
	}
	if capability.Credentials.AccountID != "account-1" || capability.Credentials.AccessToken == "" {
		t.Fatal("resolved credentials are incomplete")
	}
}

func TestOfficialRealtimeCapabilityRejectsMismatchedCurrentWhenSnapshotExpired(t *testing.T) {
	now := time.Now()
	bound := realtimeTestAuthJSON(t, "account-bound", "bound@example.com", now.Add(-time.Hour))
	runtime, settings, codexHome := realtimeTestRuntime(t, bound, "https://relay.invalid/v1")
	current := realtimeTestAuthJSON(t, "account-current", "current@example.com", now.Add(time.Hour))
	writeTestFile(t, filepath.Join(codexHome, "auth.json"), current)

	capability := runtime.officialRealtimeCapability(settings)
	if capability.Available || capability.Reason != officialRealtimeAccountMismatchReason {
		t.Fatalf("mismatched current auth should fail closed: %#v", capability)
	}
}

func TestOfficialRealtimeCapabilityRequiresActiveProfileBinding(t *testing.T) {
	valid := realtimeTestAuthJSON(t, "account-other", "other@example.com", time.Now().Add(time.Hour))
	runtime, settings, _ := realtimeTestRuntime(t, "", "https://relay.invalid/v1")
	settings.RelayProfiles = append(settings.RelayProfiles, relayProfile{
		ID: "other", RelayMode: "mixedApi", Protocol: "responses", OfficialAuthContents: valid,
	})
	if err := saveSettings(settings); err != nil {
		t.Fatalf("save settings failed: %v", err)
	}

	capability := runtime.officialRealtimeCapability(settings)
	if capability.Available || capability.Reason != officialRealtimeNotBoundReason {
		t.Fatalf("non-active binding must not enable realtime: %#v", capability)
	}
}

func TestOfficialRealtimeHTTPUsesOfficialSubscriptionOnly(t *testing.T) {
	var thirdPartyCalls atomic.Int32
	thirdParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		thirdPartyCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer thirdParty.Close()

	type capturedRequest struct {
		Authorization string
		AccountID     string
		Alpha         string
		SessionID     string
		ThreadID      string
		Query         string
		Body          map[string]any
	}
	captured := make(chan capturedRequest, 1)
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		captured <- capturedRequest{
			Authorization: req.Header.Get("authorization"),
			AccountID:     req.Header.Get("chatgpt-account-id"),
			Alpha:         req.Header.Get("openai-alpha"),
			SessionID:     req.Header.Get("session-id"),
			ThreadID:      req.Header.Get("thread-id"),
			Query:         req.URL.RawQuery,
			Body:          body,
		}
		w.Header().Set("content-type", "application/sdp")
		w.Header().Set("location", "/v1/live/rtc-official")
		w.Header().Set("openai-request-id", "req-official")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("v=0\r\nanswer"))
	}))
	defer official.Close()
	previousEndpoint := officialRealtimeCallsEndpoint
	officialRealtimeCallsEndpoint = official.URL + "/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas"
	t.Cleanup(func() { officialRealtimeCallsEndpoint = previousEndpoint })

	auth := realtimeTestAuthJSON(t, "account-official", "voice@example.com", time.Now().Add(time.Hour))
	runtime, _, _ := realtimeTestRuntime(t, auth, thirdParty.URL+"/v1")
	var multipartBody bytes.Buffer
	writer := multipart.NewWriter(&multipartBody)
	sdpPart, _ := writer.CreateFormField("sdp")
	_, _ = sdpPart.Write([]byte("v=0\r\noffer"))
	sessionPart, _ := writer.CreateFormField("session")
	_, _ = sessionPart.Write([]byte(`{"type":"quicksilver","model":"gpt-live"}`))
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/live?ignored=1", bytes.NewReader(multipartBody.Bytes()))
	request.Header.Set("content-type", writer.FormDataContentType())
	request.Header.Set("authorization", "Bearer third-party-text-key")
	request.Header.Set("chatgpt-account-id", "wrong-account")
	request.Header.Set("openai-alpha", "quicksilver=v2")
	request.Header.Set("session-id", "session-123")
	request.Header.Set("thread-id", "thread-456")
	recorder := httptest.NewRecorder()

	runtime.forwardOfficialRealtimeHTTP(recorder, request, multipartBody.Bytes())

	if recorder.Code != http.StatusCreated || recorder.Header().Get("location") != "/v1/live/rtc-official" || recorder.Body.String() != "v=0\r\nanswer" {
		t.Fatalf("official realtime response = HTTP %d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	got := <-captured
	if got.AccountID != "account-official" || !strings.HasPrefix(got.Authorization, "Bearer ") || strings.Contains(got.Authorization, "third-party") {
		t.Fatalf("official auth headers were not isolated: account=%q authorization=%q", got.AccountID, got.Authorization)
	}
	if got.Alpha != "quicksilver=v2" || got.SessionID != "session-123" || got.ThreadID != "thread-456" {
		t.Fatalf("official headers were not preserved: %#v", got)
	}
	if got.Query != "intent=quicksilver&architecture=avas" {
		t.Fatalf("official query = %q", got.Query)
	}
	if got.Body["sdp"] != "v=0\r\noffer" {
		t.Fatalf("official SDP payload = %#v", got.Body)
	}
	if session, ok := got.Body["session"].(map[string]any); !ok || session["model"] != "gpt-live" {
		t.Fatalf("official session payload = %#v", got.Body["session"])
	}
	if thirdPartyCalls.Load() != 0 {
		t.Fatalf("voice request reached third-party text relay %d times", thirdPartyCalls.Load())
	}
}

func TestOfficialRealtimeHTTPWithoutBindingMakesNoUpstreamRequest(t *testing.T) {
	var calls atomic.Int32
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer official.Close()
	previousEndpoint := officialRealtimeCallsEndpoint
	officialRealtimeCallsEndpoint = official.URL
	t.Cleanup(func() { officialRealtimeCallsEndpoint = previousEndpoint })
	runtime, _, _ := realtimeTestRuntime(t, "", "https://relay.invalid/v1")
	request := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader("unused"))
	request.Header.Set("content-type", "application/sdp")
	recorder := httptest.NewRecorder()

	runtime.forwardOfficialRealtimeHTTP(recorder, request, []byte("v=0\r\noffer"))

	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), officialRealtimeNotBoundReason) {
		t.Fatalf("missing binding response = HTTP %d body=%q", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("official upstream should not be called without a binding, calls=%d", calls.Load())
	}
}

func TestRelayProxyHandlerRoutesLiveOnlyToOfficialRealtime(t *testing.T) {
	var officialCalls atomic.Int32
	var thirdPartyCalls atomic.Int32
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		officialCalls.Add(1)
		w.Header().Set("content-type", "application/sdp")
		_, _ = w.Write([]byte("official-answer"))
	}))
	defer official.Close()
	thirdParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		thirdPartyCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer thirdParty.Close()
	previousEndpoint := officialRealtimeCallsEndpoint
	officialRealtimeCallsEndpoint = official.URL
	t.Cleanup(func() { officialRealtimeCallsEndpoint = previousEndpoint })
	auth := realtimeTestAuthJSON(t, "account-fixed-port", "fixed-port@example.com", time.Now().Add(time.Hour))
	runtime, _, _ := realtimeTestRuntime(t, auth, thirdParty.URL+"/v1")
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:57323/v1/live", strings.NewReader("v=0\r\noffer"))
	request.Header.Set("content-type", "application/sdp")
	recorder := httptest.NewRecorder()

	runtime.handleRelayProxyHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Body.String() != "official-answer" {
		t.Fatalf("fixed relay realtime response = HTTP %d body=%q", recorder.Code, recorder.Body.String())
	}
	if officialCalls.Load() != 1 || thirdPartyCalls.Load() != 0 {
		t.Fatalf("fixed relay routing calls: official=%d third_party=%d", officialCalls.Load(), thirdPartyCalls.Load())
	}
}

func TestOfficialRealtimeForbiddenIsCachedByCredential(t *testing.T) {
	var calls atomic.Int32
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"Live is not enabled for this group"}}`))
	}))
	defer official.Close()
	previousEndpoint := officialRealtimeCallsEndpoint
	officialRealtimeCallsEndpoint = official.URL
	t.Cleanup(func() { officialRealtimeCallsEndpoint = previousEndpoint })
	auth := realtimeTestAuthJSON(t, "account-denied", "denied@example.com", time.Now().Add(time.Hour))
	runtime, _, _ := realtimeTestRuntime(t, auth, "https://relay.invalid/v1")

	for index := 0; index < 2; index++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader("v=0\r\noffer"))
		request.Header.Set("content-type", "application/sdp")
		recorder := httptest.NewRecorder()
		runtime.forwardOfficialRealtimeHTTP(recorder, request, []byte("v=0\r\noffer"))
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("attempt %d response status = %d", index+1, recorder.Code)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("forbidden capability should be cached, upstream calls=%d", calls.Load())
	}
	status := runtime.officialRealtimeStatusValue()
	if boolFromAny(status["available"]) || stringFromAny(status["reason"]) != officialRealtimeNotEntitledReason {
		t.Fatalf("cached status = %#v", status)
	}
	managerStatus := relayStatusFromHome(codexHomeDir(), runtime.runtimeSettingsSnapshot())
	if boolFromAny(managerStatus["officialRealtimeAvailable"]) || stringFromAny(managerStatus["officialRealtimeReason"]) != officialRealtimeNotEntitledReason {
		t.Fatalf("manager cached status = %#v", managerStatus)
	}
	denialContents := readFile(officialRealtimeDenialPath())
	credentials, _ := parseOfficialRealtimeCredentials(auth, "test", time.Now())
	for _, secret := range []string{"account-denied", credentials.AccessToken, auth} {
		if strings.Contains(denialContents, secret) {
			t.Fatal("official realtime denial cache contains raw credentials")
		}
	}

	refreshed := realtimeTestAuthJSON(t, "account-denied", "denied@example.com", time.Now().Add(2*time.Hour))
	writeTestFile(t, filepath.Join(codexHomeDir(), "auth.json"), refreshed)
	updated := runtime.officialRealtimeCapability(runtime.runtimeSettingsSnapshot())
	if !updated.Available || updated.Reason != officialRealtimeAvailableReason {
		t.Fatalf("updated official token should invalidate denial cache: %#v", updated)
	}
}

func TestOfficialRealtimeUsageLimitIsCachedWithoutRawError(t *testing.T) {
	var calls atomic.Int32
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"You've hit your usage limit. Upgrade to Pro, visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Aug 8th, 2026 1:59 PM."}}`))
	}))
	defer official.Close()
	previousEndpoint := officialRealtimeCallsEndpoint
	officialRealtimeCallsEndpoint = official.URL
	t.Cleanup(func() { officialRealtimeCallsEndpoint = previousEndpoint })
	auth := realtimeTestAuthJSON(t, "account-limited", "limited@example.com", time.Now().Add(time.Hour))
	runtime, _, _ := realtimeTestRuntime(t, auth, "https://relay.invalid/v1")

	for index := 0; index < 2; index++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader("v=0\r\noffer"))
		request.Header.Set("content-type", "application/sdp")
		recorder := httptest.NewRecorder()
		runtime.forwardOfficialRealtimeHTTP(recorder, request, []byte("v=0\r\noffer"))
		if index == 0 && recorder.Code != http.StatusTooManyRequests {
			t.Fatalf("first usage-limited response status = %d", recorder.Code)
		}
		if index == 0 {
			body := recorder.Body.String()
			for _, expected := range []string{"语音无法开启", "官方账号的语音额度已耗尽", "Aug 8th, 2026 1:59 PM", "https://chatgpt.com/codex/settings/usage"} {
				if !strings.Contains(body, expected) {
					t.Fatalf("localized usage-limit response missing %q: %s", expected, body)
				}
			}
			if strings.Contains(body, "You've hit your usage limit") {
				t.Fatalf("localized usage-limit response leaked raw English message: %s", body)
			}
			if contentType := recorder.Header().Get("content-type"); !strings.Contains(contentType, "application/json") {
				t.Fatalf("localized usage-limit content type = %q", contentType)
			}
		}
		if index == 1 && recorder.Code != http.StatusForbidden {
			t.Fatalf("cached usage-limited response status = %d", recorder.Code)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("usage limit should stop repeated official requests, calls=%d", calls.Load())
	}
	status := runtime.officialRealtimeStatusValue()
	if boolFromAny(status["available"]) || stringFromAny(status["reason"]) != officialRealtimeUsageLimitedReason {
		t.Fatalf("usage-limited status = %#v", status)
	}
	denialContents := readFile(officialRealtimeDenialPath())
	for _, sensitive := range []string{"You've hit your usage limit", "limited@example.com", "account-limited"} {
		if strings.Contains(denialContents, sensitive) {
			t.Fatalf("usage-limit cache contains raw upstream or account data: %q", sensitive)
		}
	}
}

func TestOfficialRealtimeWebSocketUsesOfficialCredentials(t *testing.T) {
	type capturedRequest struct {
		Authorization string
		AccountID     string
		Alpha         string
		SessionID     string
		Query         string
	}
	captured := make(chan capturedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		captured <- capturedRequest{
			Authorization: req.Header.Get("authorization"),
			AccountID:     req.Header.Get("chatgpt-account-id"),
			Alpha:         req.Header.Get("openai-alpha"),
			SessionID:     req.Header.Get("x-session-id"),
			Query:         req.URL.RawQuery,
		}
		connection, err := (&websocket.Upgrader{
			CheckOrigin:  func(*http.Request) bool { return true },
			Subprotocols: []string{"realtime"},
		}).Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		messageType, data, err := connection.ReadMessage()
		if err == nil {
			_ = connection.WriteMessage(messageType, append([]byte("official:"), data...))
		}
	}))
	defer upstream.Close()
	previousWebSocketURL := officialRealtimeWebSocketURL
	officialRealtimeWebSocketURL = "ws" + strings.TrimPrefix(upstream.URL, "http") + "/v1/realtime"
	t.Cleanup(func() { officialRealtimeWebSocketURL = previousWebSocketURL })

	auth := realtimeTestAuthJSON(t, "account-ws", "ws@example.com", time.Now().Add(time.Hour))
	runtime, _, _ := realtimeTestRuntime(t, auth, "https://third-party.invalid/v1")
	local := httptest.NewServer(http.HandlerFunc(runtime.forwardOfficialRealtimeWebSocket))
	defer local.Close()
	dialer := websocket.Dialer{Subprotocols: []string{"realtime"}, HandshakeTimeout: 3 * time.Second}
	connection, response, err := dialer.Dial("ws"+strings.TrimPrefix(local.URL, "http")+"/v1/realtime?intent=quicksilver&call_id=rtc-test", http.Header{
		"Authorization": []string{"Bearer third-party-text-key"},
		"OpenAI-Alpha":  []string{"quicksilver=v2"},
		"X-Session-ID":  []string{"session-ws"},
	})
	if err != nil {
		if response != nil {
			t.Fatalf("local WebSocket handshake failed: %v (HTTP %d)", err, response.StatusCode)
		}
		t.Fatalf("local WebSocket handshake failed: %v", err)
	}
	defer connection.Close()
	if err := connection.WriteMessage(websocket.BinaryMessage, []byte("audio")); err != nil {
		t.Fatalf("write WebSocket frame failed: %v", err)
	}
	messageType, data, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read WebSocket frame failed: %v", err)
	}
	if messageType != websocket.BinaryMessage || string(data) != "official:audio" {
		t.Fatalf("WebSocket response = type %d body %q", messageType, string(data))
	}
	got := <-captured
	if got.AccountID != "account-ws" || strings.Contains(got.Authorization, "third-party") || !strings.HasPrefix(got.Authorization, "Bearer ") {
		t.Fatalf("WebSocket auth was not isolated: %#v", got)
	}
	if got.Alpha != "quicksilver=v2" || got.SessionID != "session-ws" || got.Query != "intent=quicksilver&call_id=rtc-test" {
		t.Fatalf("WebSocket metadata = %#v", got)
	}
}

func TestRelayConfigPinsRealtimeToOfficialLocalProxy(t *testing.T) {
	settings := defaultSettings()
	settings.RelayCommonConfigContents = `experimental_realtime_ws_base_url = "https://untrusted.example/v1"`
	profile := relayProfile{
		ID: "mixed", RelayMode: "mixedApi", Protocol: "responses", UseCommonConfig: true,
	}
	config, err := relayConfigWithCommonAndLimits(settings, profile, `model = "gpt-test"`)
	if err != nil {
		t.Fatalf("build relay config failed: %v", err)
	}
	for _, key := range []string{"experimental_realtime_webrtc_call_base_url", "experimental_realtime_ws_base_url"} {
		if strings.Count(config, key+" = ") != 1 || !strings.Contains(config, key+` = "`+officialRealtimeLocalBaseURL+`"`) {
			t.Fatalf("relay config should pin %s exactly once:\n%s", key, config)
		}
	}
	official := officialRelayConfigSnapshot(config)
	if strings.Contains(official, "experimental_realtime_webrtc_call_base_url") || strings.Contains(official, "experimental_realtime_ws_base_url") {
		t.Fatalf("official config should remove local realtime overrides:\n%s", official)
	}
	settings.RelayProfiles = []relayProfile{{ID: "custom", RelayMode: "pureApi", Protocol: "responses"}}
	settings.ActiveRelayID = "custom"
	if !activeRelayNeedsLocalProxy(settings) {
		t.Fatal("custom relay mode must start the official realtime local proxy")
	}
	settings.RelayProfiles[0].RelayMode = "official"
	if activeRelayNeedsLocalProxy(settings) {
		t.Fatal("official mode should use native official realtime routing")
	}
}

func TestOfficialRealtimeBridgeStatusAndInjectionGate(t *testing.T) {
	auth := realtimeTestAuthJSON(t, "account-bridge", "bridge@example.com", time.Now().Add(time.Hour))
	runtime, _, _ := realtimeTestRuntime(t, auth, "https://relay.invalid/v1")
	status := runtime.handleBridgeRequest("/realtime/status", json.RawMessage(`{}`))
	if status["status"] != "ok" || !boolFromAny(status["available"]) || status["reason"] != officialRealtimeAvailableReason {
		t.Fatalf("realtime bridge status = %#v", status)
	}
	settings := runtime.handleBridgeRequest("/settings/get", json.RawMessage(`{}`))
	if !boolFromAny(settings["officialRealtimeAvailable"]) || stringFromAny(settings["officialRealtimeMessage"]) == "" {
		t.Fatalf("settings realtime fields = %#v", settings)
	}
	script, err := os.ReadFile(filepath.Join("assets", "inject", "renderer-inject.js"))
	if err != nil {
		t.Fatalf("read renderer injection failed: %v", err)
	}
	for _, marker := range []string{
		`postJson("/realtime/status"`,
		`officialRealtimeVoiceIconPathPrefix`,
		`officialRealtimeClickBypass`,
		`officialRealtimeClickPending`,
		`event.stopImmediatePropagation()`,
		`installOfficialRealtimeVoiceGate()`,
	} {
		if !strings.Contains(string(script), marker) {
			t.Fatalf("renderer injection missing official realtime gate marker %q", marker)
		}
	}
}

func TestOfficialRealtimeDiagnosticsDoNotContainCredentials(t *testing.T) {
	auth := realtimeTestAuthJSON(t, "account-secret-marker", "secret@example.com", time.Now().Add(time.Hour))
	runtime, _, _ := realtimeTestRuntime(t, auth, "https://relay.invalid/v1")
	previousEndpoint := officialRealtimeCallsEndpoint
	officialRealtimeCallsEndpoint = "http://127.0.0.1:1/unreachable"
	t.Cleanup(func() { officialRealtimeCallsEndpoint = previousEndpoint })
	request := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader("v=0\r\noffer"))
	request.Header.Set("content-type", "application/sdp")
	recorder := httptest.NewRecorder()
	runtime.forwardOfficialRealtimeHTTP(recorder, request, []byte("v=0\r\noffer"))
	logContents := readFile(diagnosticLogPath())
	credentials, _ := parseOfficialRealtimeCredentials(auth, "test", time.Now())
	for _, secret := range []string{"account-secret-marker", credentials.AccessToken, auth, "v=0\r\noffer"} {
		if secret != "" && strings.Contains(logContents, secret) {
			t.Fatalf("diagnostic log leaked official realtime credential or payload")
		}
	}
}
