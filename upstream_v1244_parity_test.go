package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestModelsEndpointPreservesVersionedBaseURL(t *testing.T) {
	for _, test := range []struct {
		base string
		want string
	}{
		{base: "https://ark.example/api/coding/v3", want: "https://ark.example/api/coding/v3/models"},
		{base: "https://api.example/v1", want: "https://api.example/v1/models"},
		{base: "https://api.example", want: "https://api.example/v1/models"},
	} {
		if got := modelsEndpoint(test.base); got != test.want {
			t.Fatalf("modelsEndpoint(%q) = %q, want %q", test.base, got, test.want)
		}
	}
}

func TestResponsesRequestSkipsContentlessMetadata(t *testing.T) {
	converted, err := responsesToChatCompletions(map[string]any{
		"model":        "deepseek-chat",
		"instructions": "Be precise.",
		"input": []any{
			map[string]any{
				"type":  "additional_tools",
				"role":  "developer",
				"tools": []any{map[string]any{"type": "custom", "name": "exec"}},
			},
			map[string]any{
				"type":    "message",
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("convert request: %v", err)
	}
	messages, _ := converted["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("contentless metadata became a chat message: %#v", messages)
	}
	for _, raw := range messages {
		message := raw.(map[string]any)
		if message["content"] == nil {
			t.Fatalf("converted message has nil content: %#v", message)
		}
	}
}

func TestProviderURLImportDiscardsEmbeddedConfigAndAuth(t *testing.T) {
	values := url.Values{
		"name":           {"Unsafe Provider"},
		"baseUrl":        {"https://relay.example/v1"},
		"apiKey":         {"sk-test"},
		"configContents": {"not-valid-base64"},
		"authContents":   {"also-not-valid-base64"},
	}
	request, err := providerImportRequestFromURL("codexplusplus://v1/import/provider?" + values.Encode())
	if err != nil {
		t.Fatalf("untrusted embedded files should be ignored, got %v", err)
	}
	if strings.Contains(request.ConfigContents, "not-valid-base64") || strings.Contains(request.AuthContents, "also-not-valid-base64") {
		t.Fatalf("embedded provider files survived normalization: %#v", request)
	}
	if !strings.Contains(request.ConfigContents, `base_url = "https://relay.example/v1"`) || !strings.Contains(request.AuthContents, `"OPENAI_API_KEY": "sk-test"`) {
		t.Fatalf("managed provider snapshots were not generated: %#v", request)
	}
}

func TestPendingProviderImportDoesNotPersistExecutableFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	request := providerImportRequest{
		Name:           "Unsafe Provider",
		BaseURL:        "https://relay.example/v1",
		APIKey:         "sk-test",
		WireAPI:        "responses",
		RelayMode:      "pureApi",
		ConfigContents: "notify = [\"calc\"]\n[mcp_servers.evil]\ncommand = \"cmd\"\n",
		AuthContents:   `{"OPENAI_API_KEY":"sk-test","exec":"calc"}`,
	}
	if err := savePendingProviderImport(request); err != nil {
		t.Fatalf("save pending import: %v", err)
	}
	data, err := os.ReadFile(pendingProviderImportPath())
	if err != nil {
		t.Fatalf("read pending import: %v", err)
	}
	for _, forbidden := range []string{"notify", "mcp_servers", `"exec"`} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("pending import persisted untrusted content %q: %s", forbidden, data)
		}
	}
	loaded, err := loadPendingProviderImport()
	if err != nil || loaded == nil {
		t.Fatalf("load pending import: %#v %v", loaded, err)
	}
	if strings.Contains(loaded.ConfigContents, "notify") || strings.Contains(loaded.AuthContents, "exec") {
		t.Fatalf("loaded import restored untrusted content: %#v", loaded)
	}
}

func TestCommonRelayConfigExcludesProviderCredentials(t *testing.T) {
	config := `model = "gpt-test"
model_provider = "custom"
base_url = "https://root.example/v1"
openai_base_url = "https://openai.example/v1"
chatgpt_base_url = "https://chatgpt.example/backend-api"
OPENAI_API_KEY = "sk-openai"
CUSTOM_API_KEY = "sk-custom"
experimental_bearer_token = "sk-bearer"
model_reasoning_effort = "high"

[model_providers.custom]
base_url = "https://relay.example/v1"
experimental_bearer_token = "sk-table"

[features]
goals = true
`
	common := extractCommonRelayConfig(config)
	for _, forbidden := range []string{"model_provider", "openai_base_url", "chatgpt_base_url", "OPENAI_API_KEY", "CUSTOM_API_KEY", "experimental_bearer_token", "[model_providers.custom]"} {
		if strings.Contains(common, forbidden) {
			t.Fatalf("common config leaked %q:\n%s", forbidden, common)
		}
	}
	for _, expected := range []string{`model_reasoning_effort = "high"`, "[features]", "goals = true"} {
		if !strings.Contains(common, expected) {
			t.Fatalf("common config lost %q:\n%s", expected, common)
		}
	}
	profile := stripCommonConfigFromConfig(config, common)
	for _, expected := range []string{"openai_base_url", "CUSTOM_API_KEY", "experimental_bearer_token", "[model_providers.custom]"} {
		if !strings.Contains(profile, expected) {
			t.Fatalf("profile config lost provider field %q:\n%s", expected, profile)
		}
	}
}

func TestResilientLoopbackGuardFallsBackForForbiddenPort(t *testing.T) {
	guard, err := acquireResilientLoopbackPortGuardWith(
		57320,
		t.TempDir(),
		func(uint16) (net.Listener, error) { return nil, fmt.Errorf("listen: %w", syscall.EACCES) },
		func(uint16) bool { return true },
	)
	if err != nil {
		t.Fatalf("forbidden reserved port should use file lock fallback: %v", err)
	}
	defer guard.release()
	if guard.listener != nil || guard.fallbackPath() == "" {
		t.Fatalf("unexpected forbidden-port guard: %#v", guard)
	}
}

func TestQuickChatRendererIsNotSelectedForInjection(t *testing.T) {
	quick := cdpTarget{
		ID:                   "quick",
		Type:                 "page",
		Title:                "ChatGPT",
		URL:                  "app://-/index.html?initialRoute=%2Fchatgpt%2Fquick-chat-prewarm",
		WebSocketDebuggerURL: "ws://127.0.0.1:9229/devtools/page/quick",
	}
	main := cdpTarget{
		ID:                   "main",
		Type:                 "page",
		Title:                "ChatGPT",
		URL:                  "app://-/index.html",
		WebSocketDebuggerURL: "ws://127.0.0.1:9229/devtools/page/main",
	}
	if !isQuickChatCDPPageTarget(quick) || isPrimaryCodexCDPPageTarget(quick) {
		t.Fatalf("quick chat target classification failed: %#v", quick)
	}
	selected, err := pickCDPPageTarget([]cdpTarget{quick, main})
	if err != nil || selected.ID != "main" {
		t.Fatalf("primary target selection = %#v, %v", selected, err)
	}
	if _, err := pickCDPPageTarget([]cdpTarget{quick}); err == nil {
		t.Fatal("quick-chat-only target list should not be injectable")
	}
}

func TestRendererModelPatchUsesNarrowUpstreamScope(t *testing.T) {
	for _, required := range []string{
		"modelJsonResponseLooksPatchable",
		`String(name) === "107580212"`,
		`window.addEventListener("codex-message-from-view"`,
		"codexPlusModelListRequestIds.size === 0",
	} {
		if !strings.Contains(rendererInjectScript, required) {
			t.Fatalf("renderer injection missing narrowed model patch marker %q", required)
		}
	}
	for _, forbidden := range []string{
		"function patchReactModelState",
		"function patchObjectGraphForModels",
		"window.dispatchEvent = function patchedCodexPlusDispatchEvent",
	} {
		if strings.Contains(rendererInjectScript, forbidden) {
			t.Fatalf("renderer injection still contains broad model patch %q", forbidden)
		}
	}
}
