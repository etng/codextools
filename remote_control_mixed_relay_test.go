package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
)

func TestRemoteControlOpenAIBaseURLFollowsOfficialMixMode(t *testing.T) {
	managed := "http://127.0.0.1:57323/v1"
	mixed := relayProfile{RelayMode: "mixedApi", OfficialMixAPIKey: true}

	config := applyRemoteControlOpenAIBaseURL(`model = "gpt-test"`, mixed)
	if strings.Count(config, "openai_base_url = ") != 1 || rootKeyString(config, "openai_base_url") != managed {
		t.Fatalf("mixed config must contain one managed openai_base_url:\n%s", config)
	}
	config = applyRemoteControlOpenAIBaseURL(config, mixed)
	if strings.Count(config, "openai_base_url = ") != 1 {
		t.Fatalf("managed openai_base_url must be idempotent:\n%s", config)
	}

	custom := `openai_base_url = "https://user-openai.example/v1"
model = "gpt-test"`
	if got := applyRemoteControlOpenAIBaseURL(custom, mixed); rootKeyString(got, "openai_base_url") != "https://user-openai.example/v1" {
		t.Fatalf("mixed mode replaced a user openai_base_url:\n%s", got)
	}

	pure := applyRemoteControlOpenAIBaseURL(config, relayProfile{RelayMode: "pureApi"})
	if strings.Contains(pure, managed) {
		t.Fatalf("pure API mode retained managed openai_base_url:\n%s", pure)
	}
	official := officialRelayConfigSnapshot(config)
	if strings.Contains(official, managed) {
		t.Fatalf("official mode retained managed openai_base_url:\n%s", official)
	}
}

func TestRelayConfigWritesManagedRemoteControlBaseURLOnlyForMixedMode(t *testing.T) {
	settings := defaultSettings()
	profile := relayProfile{
		ID: "mixed", RelayMode: "mixedApi", OfficialMixAPIKey: true, Protocol: "responses",
		UseCommonConfig: true,
	}
	config, err := relayConfigWithCommonAndLimits(settings, profile, `model_provider = "CodexPlusPlus"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := rootKeyString(config, "openai_base_url"); got != "http://127.0.0.1:57323/v1" {
		t.Fatalf("mixed remote-control base URL = %q\n%s", got, config)
	}
	profile.RelayMode = "pureApi"
	profile.OfficialMixAPIKey = false
	config, err = relayConfigWithCommonAndLimits(settings, profile, config)
	if err != nil {
		t.Fatal(err)
	}
	if got := rootKeyString(config, "openai_base_url"); got != "" {
		t.Fatalf("pure API config retained managed remote-control base URL = %q\n%s", got, config)
	}
}

func TestDecodeProtocolProxyRequestBodyZstd(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"probe","stream":false}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(body, nil)
	encoder.Close()
	decoded, err := decodeProtocolProxyRequestBody(compressed, "zstd")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, body) {
		t.Fatalf("decoded body = %q", decoded)
	}
	if _, err := decodeProtocolProxyRequestBody(body, "gzip"); err == nil {
		t.Fatal("unsupported compression must fail closed")
	}
}

func TestMixedRemoteControlZstdResponsesReachConfiguredProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var upstreamBody []byte
	var upstreamAuthorization string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		upstreamAuthorization = req.Header.Get("authorization")
		upstreamBody, _ = io.ReadAll(req.Body)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_remote","object":"response"}`))
	}))
	defer provider.Close()

	settings := defaultSettings()
	settings.RelayProfiles = []relayProfile{{
		ID: "mixed", Name: "Mixed", RelayMode: "mixedApi", OfficialMixAPIKey: true,
		Protocol: "responses", BaseURL: provider.URL + "/v1", UpstreamBaseURL: provider.URL + "/v1", APIKey: "sk-provider",
		ImageGenerationEnabled: true,
	}}
	settings.ActiveRelayID = "mixed"
	runtime := &launcherRuntime{settings: settings}

	body := []byte(`{"model":"gpt-5.6-sol","input":"probe","stream":false}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(body, nil)
	encoder.Close()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:57323/v1/responses", bytes.NewReader(compressed))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("content-encoding", "zstd")
	req.Header.Set("authorization", "Bearer chatgpt-secret")
	recorder := httptest.NewRecorder()
	runtime.handleRelayProxyHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if upstreamAuthorization != "Bearer sk-provider" {
		t.Fatalf("upstream authorization = %q", upstreamAuthorization)
	}
	var request map[string]any
	if err := json.Unmarshal(upstreamBody, &request); err != nil || request["model"] != "gpt-5.6-sol" {
		t.Fatalf("upstream body = %q err=%v", upstreamBody, err)
	}
}

func TestResponsesWebSocketRequestsFallBackToHTTP(t *testing.T) {
	runtime := &launcherRuntime{settings: defaultSettings()}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:57323/v1/responses", nil)
	req.Header.Set("connection", "Upgrade")
	req.Header.Set("upgrade", "websocket")
	if !websocket.IsWebSocketUpgrade(req) {
		t.Fatal("test request is not a WebSocket upgrade")
	}
	recorder := httptest.NewRecorder()
	runtime.handleRelayProxyHTTP(recorder, req)
	if recorder.Code != http.StatusUpgradeRequired || !strings.Contains(recorder.Body.String(), "protocol_proxy_http_only") {
		t.Fatalf("upgrade response = HTTP %d %s", recorder.Code, recorder.Body.String())
	}
}
