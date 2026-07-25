package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesToChatCompletionsConvertsMessagesToolsAndReasoning(t *testing.T) {
	request := map[string]any{
		"model":               "deepseek-r1",
		"instructions":        "Be precise",
		"max_output_tokens":   float64(128),
		"stream":              true,
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": "high"},
		"input": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"go"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "result"},
		},
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "description": "Search", "parameters": map[string]any{"type": "object"}}},
	}

	converted, err := responsesToChatCompletions(request)
	if err != nil {
		t.Fatalf("convert request: %v", err)
	}
	if converted["model"] != "deepseek-r1" || converted["max_tokens"] != float64(128) {
		t.Fatalf("model/token conversion mismatch: %#v", converted)
	}
	if converted["enable_thinking"] != true || converted["reasoning_effort"] != "high" {
		t.Fatalf("reasoning conversion mismatch: %#v", converted)
	}
	streamOptions, _ := converted["stream_options"].(map[string]any)
	if streamOptions["include_usage"] != true {
		t.Fatalf("stream usage was not requested: %#v", converted)
	}
	messages, _ := converted["messages"].([]any)
	if len(messages) != 4 || stringFromAny(messages[0].(map[string]any)["role"]) != "system" || stringFromAny(messages[3].(map[string]any)["role"]) != "tool" {
		t.Fatalf("message conversion mismatch: %#v", messages)
	}
	tools, _ := converted["tools"].([]any)
	if len(tools) != 1 || stringFromAny(tools[0].(map[string]any)["type"]) != "function" {
		t.Fatalf("tool conversion mismatch: %#v", tools)
	}
}

func TestProtocolProxyUsesChatEndpointForResponsesRequest(t *testing.T) {
	profile := relayProfile{Protocol: "chatCompletions", BaseURL: "https://api.example.test/v1"}
	body := []byte(`{"model":"gpt-test","input":"hello","stream":false}`)
	converted, ctx, err := convertResponsesRequestForProfile(profile, "/v1/responses", body)
	if err != nil {
		t.Fatalf("convert request: %v", err)
	}
	if !ctx.Converted || ctx.Stream {
		t.Fatalf("unexpected protocol context: %#v", ctx)
	}
	if got := protocolProxyTargetURL(profile, "/v1/responses", ctx.Converted); got != "https://api.example.test/v1/chat/completions" {
		t.Fatalf("chat target = %q", got)
	}
	var payload map[string]any
	if json.Unmarshal(converted, &payload) != nil || stringFromAny(payload["model"]) != "gpt-test" {
		t.Fatalf("invalid converted body: %s", converted)
	}
	if got := protocolProxyTargetURL(relayProfile{Protocol: "chatCompletions", BaseURL: "https://api.example.test/v1/chat/completions"}, "/v1/models", false); got != "https://api.example.test/v1/models" {
		t.Fatalf("models target should remove complete chat endpoint, got %q", got)
	}
}

func TestChatCompletionToResponsesPreservesReasoningToolsAndUsage(t *testing.T) {
	chat := map[string]any{
		"id": "chatcmpl_1", "created": float64(42), "model": "gpt-test",
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"content": "answer", "reasoning_content": "thought",
				"tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"go"}`}}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(4), "prompt_tokens_details": map[string]any{"cached_tokens": float64(3)}},
	}
	response, err := chatCompletionToResponse(chat, map[string]any{"service_tier": "priority"})
	if err != nil {
		t.Fatalf("convert response: %v", err)
	}
	if response["id"] != "resp_chatcmpl_1" || response["status"] != "completed" || response["service_tier"] != "priority" {
		t.Fatalf("response envelope mismatch: %#v", response)
	}
	output, _ := response["output"].([]any)
	if len(output) != 3 || stringFromAny(output[0].(map[string]any)["type"]) != "reasoning" || stringFromAny(output[2].(map[string]any)["type"]) != "function_call" {
		t.Fatalf("response output mismatch: %#v", output)
	}
	usage, _ := response["usage"].(map[string]any)
	if uint64FromAny(usage["input_tokens"], 0) != 7 || uint64FromAny(usage["total_tokens"], 0) != 14 {
		t.Fatalf("usage conversion mismatch: %#v", usage)
	}
}

func TestChatSSEToResponsesStreamsTextReasoningToolsAndCompletion(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chatcmpl_stream","model":"gpt-test","created":42,"choices":[{"delta":{"reasoning_content":"think "},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_stream","choices":[{"delta":{"content":"answer"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_stream","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\\\"q\\\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_stream","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\\\"go\\\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	var output bytes.Buffer
	if _, err := streamChatSSEAsResponses(&output, strings.NewReader(input), map[string]any{"model": "gpt-test"}); err != nil {
		t.Fatalf("convert stream: %v", err)
	}
	text := output.String()
	for _, marker := range []string{"event: response.created", "event: response.reasoning_summary_text.delta", "event: response.output_text.delta", "event: response.function_call_arguments.delta", "event: response.function_call_arguments.done", "event: response.completed", "data: [DONE]"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("stream missing %q:\n%s", marker, text)
		}
	}
}

func TestChatSSEErrorBecomesResponsesFailure(t *testing.T) {
	input := "event: error\ndata: {\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit\"}}\n\n"
	var output bytes.Buffer
	if _, err := streamChatSSEAsResponses(&output, strings.NewReader(input), nil); err != nil {
		t.Fatalf("convert stream error: %v", err)
	}
	if !strings.Contains(output.String(), "event: response.failed") || strings.Contains(output.String(), "event: response.completed") {
		t.Fatalf("unexpected error stream:\n%s", output.String())
	}
}

func TestResponsesErrorConversionPreservesStructuredFields(t *testing.T) {
	converted := responsesErrorFromUpstream(429, "application/json", []byte(`{"error":{"message":"slow down","type":"rate_limit","code":"quota"}}`))
	errorValue, _ := converted["error"].(map[string]any)
	if errorValue["message"] != "slow down" || errorValue["type"] != "rate_limit" || errorValue["code"] != "quota" {
		t.Fatalf("error conversion mismatch: %#v", converted)
	}
}

func TestResponsesRequestMapsCustomNamespaceAndApplyPatchTools(t *testing.T) {
	request := map[string]any{
		"model": "gpt-test", "input": "hi", "stream": true,
		"tools": []any{
			map[string]any{"type": "custom", "name": "exec", "description": "Run command"},
			map[string]any{"type": "namespace", "name": "mcp__vscode_mcp__", "description": "VS Code", "tools": []any{
				map[string]any{"type": "function", "name": "open_file", "parameters": map[string]any{}},
			}},
			map[string]any{"type": "custom", "name": "apply_patch", "description": "Patch files"},
			map[string]any{"type": "web_search"},
		},
		"tool_choice": map[string]any{"type": "custom", "name": "apply_patch"},
	}
	converted, err := responsesToChatCompletions(request)
	if err != nil {
		t.Fatalf("convert request: %v", err)
	}
	tools, _ := converted["tools"].([]any)
	var names []string
	for _, raw := range tools {
		tool := raw.(map[string]any)
		function := tool["function"].(map[string]any)
		names = append(names, stringFromAny(function["name"]))
		parameters := function["parameters"].(map[string]any)
		if parameters["type"] == nil || parameters["properties"] == nil || parameters["required"] == nil {
			t.Fatalf("tool parameters were not normalized: %#v", parameters)
		}
	}
	wantNames := []string{"exec", "mcp__vscode_mcp__open_file", "apply_patch_add_file", "apply_patch_delete_file", "apply_patch_update_file", "apply_patch_replace_file", "apply_patch_batch", "web_search"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("tool names = %#v, want %#v", names, wantNames)
	}
	choice := converted["tool_choice"].(map[string]any)["function"].(map[string]any)
	if choice["name"] != "apply_patch_batch" {
		t.Fatalf("apply_patch tool choice = %#v", choice)
	}
	streamOptions := converted["stream_options"].(map[string]any)
	if streamOptions["include_usage"] != true {
		t.Fatalf("stream usage missing: %#v", streamOptions)
	}
}

func TestResponsesHistorySanitizesLegacyPatchAndOrphanToolItems(t *testing.T) {
	request := map[string]any{
		"model": "gpt-test",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "bad", "name": "broken", "arguments": `{foo: "bar"}`},
			map[string]any{"type": "function_call_output", "call_id": "bad", "output": "handled"},
			map[string]any{"type": "custom_tool_call", "call_id": "patch", "name": "apply_patch", "input": "*** Begin Patch\n*** Add File: docs/test.md\n+# Test\n*** End Patch"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "missing", "output": "orphan"},
			map[string]any{"type": "tool_call", "tool_use": map[string]any{"id": "legacy", "name": "lookup", "input": map[string]any{"query": "go"}}},
			map[string]any{"type": "tool_result", "content": map[string]any{"tool_use_id": "legacy", "content": map[string]any{"ok": true}}},
		},
	}
	converted, err := responsesToChatCompletions(request)
	if err != nil {
		t.Fatalf("convert history: %v", err)
	}
	messages := converted["messages"].([]any)
	firstCall := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	arguments := firstCall["function"].(map[string]any)["arguments"].(string)
	var parsed map[string]any
	if json.Unmarshal([]byte(arguments), &parsed) != nil || parsed["input"] != `{foo: "bar"}` {
		t.Fatalf("invalid arguments were not sanitized: %q", arguments)
	}
	patchCall := messages[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if patchCall["name"] != "apply_patch_add_file" || patchCall["arguments"] != `{"content":"# Test","path":"docs/test.md"}` {
		t.Fatalf("patch history conversion mismatch: %#v", patchCall)
	}
	orphan := messages[3].(map[string]any)
	if orphan["role"] != "user" || orphan["content"] != "Function call output (missing): orphan" {
		t.Fatalf("orphan output was not downgraded: %#v", orphan)
	}
	legacy := messages[4].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if legacy["name"] != "lookup" || legacy["arguments"] != `{"query":"go"}` {
		t.Fatalf("legacy tool history mismatch: %#v", legacy)
	}
}

func TestChatResponseRestoresCustomNamespacePatchReasoningAndCacheUsage(t *testing.T) {
	original := map[string]any{"tools": []any{
		map[string]any{"type": "custom", "name": "exec"},
		map[string]any{"type": "custom", "name": "apply_patch"},
		map[string]any{"type": "namespace", "name": "mcp__vscode_mcp__", "tools": []any{map[string]any{"type": "function", "name": "open_file", "parameters": map[string]any{}}}},
	}}
	chat := map[string]any{
		"id": "chatcmpl_tools", "created": float64(123), "model": "gpt-test",
		"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
			"reasoning_details": []any{map[string]any{"summary": "Step one."}, map[string]any{"parts": []any{map[string]any{"text": "Step two."}}}},
			"tool_calls": []any{
				map[string]any{"id": "custom", "function": map[string]any{"name": "exec", "arguments": `{"input":"ls -la"}`}},
				map[string]any{"id": "ns", "function": map[string]any{"name": "mcp__vscode_mcp__open_file", "arguments": `{"path":"main.go"}`}},
				map[string]any{"id": "patch", "function": map[string]any{"name": "apply_patch_add_file", "arguments": `{"path":"README.md","content":"hello"}`}},
			},
		}}},
		"usage": map[string]any{"input_tokens": float64(10), "output_tokens": float64(3), "cache_read_input_tokens": float64(2), "cache_creation_5m_input_tokens": float64(4), "cache_creation_1h_input_tokens": float64(6)},
	}
	response, err := chatCompletionToResponse(chat, original)
	if err != nil {
		t.Fatalf("convert response: %v", err)
	}
	output := response["output"].([]any)
	if output[0].(map[string]any)["summary"].([]any)[0].(map[string]any)["text"] != "Step one.\n\nStep two." {
		t.Fatalf("reasoning details mismatch: %#v", output[0])
	}
	custom := output[1].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["name"] != "exec" || custom["input"] != "ls -la" {
		t.Fatalf("custom call mismatch: %#v", custom)
	}
	namespace := output[2].(map[string]any)
	if namespace["type"] != "function_call" || namespace["name"] != "open_file" || namespace["namespace"] != "mcp__vscode_mcp__" {
		t.Fatalf("namespace call mismatch: %#v", namespace)
	}
	patch := output[3].(map[string]any)
	if patch["name"] != "apply_patch" || patch["input"] != "*** Begin Patch\n*** Add File: README.md\n+hello\n*** End Patch" {
		t.Fatalf("patch call mismatch: %#v", patch)
	}
	usage := response["usage"].(map[string]any)
	if usage["total_tokens"] != uint64(25) || usage["cache_ttl"] != "mixed" || usage["input_tokens_details"] != nil {
		t.Fatalf("cache usage mismatch: %#v", usage)
	}
}

func TestChatSSErestoresCustomAndApplyPatchCalls(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chatcmpl_stream","choices":[{"delta":{"tool_calls":[{"index":0,"id":"exec_call","function":{"name":"exec","arguments":"{\"input\":\"ls -la\"}"}},{"index":1,"id":"patch_call","function":{"name":"apply_patch_add_file","arguments":"{\"path\":\"x.txt\",\"content\":\"hello\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"", "data: [DONE]", "",
	}, "\n")
	request := map[string]any{"tools": []any{map[string]any{"type": "custom", "name": "exec"}, map[string]any{"type": "custom", "name": "apply_patch"}}}
	var output bytes.Buffer
	if _, err := streamChatSSEAsResponses(&output, strings.NewReader(input), request); err != nil {
		t.Fatalf("convert custom stream: %v", err)
	}
	text := output.String()
	if strings.Count(text, "event: response.custom_tool_call_input.delta") != 2 || strings.Contains(text, "event: response.function_call_arguments.delta") {
		t.Fatalf("custom stream event mapping mismatch:\n%s", text)
	}
	for _, marker := range []string{`"name":"exec"`, `"input":"ls -la"`, `"name":"apply_patch"`, `*** Add File: x.txt`, `data: [DONE]`} {
		if !strings.Contains(text, marker) {
			t.Fatalf("custom stream missing %q:\n%s", marker, text)
		}
	}
}
