package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type protocolProxyContext struct {
	OriginalRequest map[string]any
	Converted       bool
	Stream          bool
}

func convertResponsesRequestForProfile(profile relayProfile, path string, body []byte) ([]byte, protocolProxyContext, error) {
	ctx := protocolProxyContext{}
	if profile.Protocol != "chatCompletions" || !isResponsesProxyPath(path) {
		return body, ctx, nil
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, ctx, fmt.Errorf("invalid Responses request: %w", err)
	}
	converted, err := responsesToChatCompletions(request)
	if err != nil {
		return nil, ctx, err
	}
	data, err := json.Marshal(converted)
	if err != nil {
		return nil, ctx, err
	}
	ctx.OriginalRequest = request
	ctx.Converted = true
	ctx.Stream = boolFromAny(request["stream"])
	return data, ctx, nil
}

func isResponsesProxyPath(path string) bool {
	path = strings.SplitN(path, "?", 2)[0]
	switch path {
	case "/responses", "/v1/responses", "/v1/v1/responses", "/codex/v1/responses":
		return true
	default:
		return false
	}
}

func protocolProxyTargetURL(profile relayProfile, requestPath string, converted bool) string {
	if converted {
		return chatCompletionsURL(effectiveUpstreamBaseURL(profile))
	}
	if isModelsProxyPath(requestPath) {
		return modelsEndpoint(effectiveUpstreamBaseURL(profile))
	}
	return relayTargetURL(relayProxyBaseURL(effectiveUpstreamBaseURL(profile), profile.Protocol), requestPath)
}

func isModelsProxyPath(path string) bool {
	path = strings.SplitN(path, "?", 2)[0]
	switch path {
	case "/models", "/v1/models", "/v1/v1/models", "/codex/v1/models":
		return true
	default:
		return false
	}
}

func chatCompletionsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(strings.ToLower(base), "/chat/completions") {
		return base
	}
	originOnly := false
	if parsed, err := http.NewRequest(http.MethodGet, base, nil); err == nil && parsed.URL != nil {
		originOnly = parsed.URL.Path == "" || parsed.URL.Path == "/"
	}
	if originOnly {
		base += "/v1"
	}
	for strings.Contains(base, "/v1/v1") {
		base = strings.ReplaceAll(base, "/v1/v1", "/v1")
	}
	return base + "/chat/completions"
}

func responsesToChatCompletions(body map[string]any) (map[string]any, error) {
	result := map[string]any{}
	copyIfPresent(result, body, "model")
	messages := make([]any, 0)
	if instructions := responseText(body["instructions"]); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	appendResponsesInput(body["input"], &messages)
	result["messages"] = collapseSystemMessages(messages)

	model := strings.ToLower(stringFromAny(body["model"]))
	if value, ok := body["max_output_tokens"]; ok {
		if strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") {
			result["max_completion_tokens"] = value
		} else {
			result["max_tokens"] = value
		}
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "temperature", "top_p", "stream", "frequency_penalty", "logit_bias", "logprobs", "metadata", "n", "presence_penalty", "response_format", "seed", "service_tier", "stop", "top_logprobs", "user"} {
		copyIfPresent(result, body, key)
	}
	if boolFromAny(body["stream"]) {
		streamOptions, _ := body["stream_options"].(map[string]any)
		streamOptions = cloneMap(streamOptions)
		streamOptions["include_usage"] = true
		result["stream_options"] = streamOptions
	} else {
		copyIfPresent(result, body, "stream_options")
	}
	applyChatReasoningOptions(result, body, model)
	toolContext := buildProtocolToolContext(body["tools"])
	tools := convertResponsesToolsToChat(body["tools"], toolContext)
	if len(tools) > 0 {
		result["tools"] = tools
		if choice := convertResponsesToolChoice(body["tool_choice"], toolContext); choice != nil {
			result["tool_choice"] = choice
		}
		copyIfPresent(result, body, "parallel_tool_calls")
	}
	return result, nil
}

func appendResponsesInput(input any, messages *[]any) {
	switch value := input.(type) {
	case string:
		*messages = append(*messages, map[string]any{"role": "user", "content": value})
	case []any:
		pendingCalls := make([]any, 0)
		pendingReasoning := make([]string, 0)
		seenCallIDs := map[string]bool{}
		flushCalls := func() {
			if len(pendingCalls) == 0 {
				return
			}
			message := map[string]any{"role": "assistant", "content": "", "tool_calls": pendingCalls}
			if len(pendingReasoning) > 0 {
				message["reasoning_content"] = strings.Join(pendingReasoning, "\n")
				pendingReasoning = nil
			}
			*messages = append(*messages, message)
			pendingCalls = nil
		}
		flushReasoning := func() {
			if len(pendingReasoning) == 0 {
				return
			}
			*messages = append(*messages, map[string]any{"role": "assistant", "content": "", "reasoning_content": strings.Join(pendingReasoning, "\n")})
			pendingReasoning = nil
		}
		appendOutput := func(callID string, output any) {
			if callID == "" {
				return
			}
			flushCalls()
			if !seenCallIDs[callID] {
				flushReasoning()
				*messages = append(*messages, map[string]any{"role": "user", "content": fmt.Sprintf("Function call output (%s): %s", callID, responseText(output))})
				return
			}
			*messages = append(*messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": responseText(output)})
		}
		for _, raw := range value {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typeName := strings.TrimSpace(stringFromAny(item["type"]))
			switch typeName {
			case "function_call":
				callID := firstString(stringFromAny(item["call_id"]), stringFromAny(item["id"]))
				name := flattenProtocolToolName(stringFromAny(item["namespace"]), stringFromAny(item["name"]))
				if callID == "" || name == "" {
					continue
				}
				seenCallIDs[callID] = true
				pendingCalls = append(pendingCalls, map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": normalizeChatToolArguments(item["arguments"])}})
			case "custom_tool_call":
				callID := firstString(stringFromAny(item["call_id"]), stringFromAny(item["id"]))
				if callID == "" {
					continue
				}
				name, arguments := buildCustomToolHistory(stringFromAny(item["name"]), firstNonNil(item["input"], item["arguments"]))
				seenCallIDs[callID] = true
				pendingCalls = append(pendingCalls, map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}})
			case "function_call_output", "custom_tool_call_output":
				appendOutput(firstString(stringFromAny(item["call_id"]), stringFromAny(item["id"])), item["output"])
			case "tool_call":
				toolUse, _ := item["tool_use"].(map[string]any)
				callID := firstString(stringFromAny(toolUse["id"]), stringFromAny(item["call_id"]), stringFromAny(item["id"]))
				name := stringFromAny(toolUse["name"])
				if callID == "" || name == "" {
					continue
				}
				seenCallIDs[callID] = true
				pendingCalls = append(pendingCalls, map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": normalizeChatToolArguments(toolUse["input"])}})
			case "tool_result":
				content, _ := item["content"].(map[string]any)
				callID := firstString(stringFromAny(content["tool_use_id"]), stringFromAny(item["tool_call_id"]), stringFromAny(item["call_id"]))
				output := item["content"]
				if content != nil && content["content"] != nil {
					output = content["content"]
				}
				appendOutput(callID, output)
			case "reasoning":
				text := reasoningText(item)
				if text != "" {
					pendingReasoning = append(pendingReasoning, text)
				}
			default:
				flushCalls()
				content, hasContent := item["content"]
				if !hasContent {
					continue
				}
				role := stringFromAny(item["role"])
				if role == "" {
					role = "user"
				}
				if content == nil && role != "assistant" {
					continue
				}
				message := map[string]any{"role": role, "content": responsesContentToChat(content)}
				if role == "assistant" && len(pendingReasoning) > 0 {
					message["reasoning_content"] = strings.Join(pendingReasoning, "\n")
					pendingReasoning = nil
				} else if role != "assistant" {
					flushReasoning()
				}
				*messages = append(*messages, message)
			}
		}
		flushCalls()
		flushReasoning()
	case map[string]any:
		appendResponsesInput([]any{value}, messages)
	}
}

func responsesContentToChat(value any) any {
	switch content := value.(type) {
	case string:
		return content
	case []any:
		parts := make([]any, 0, len(content))
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch stringFromAny(part["type"]) {
			case "input_text", "output_text", "text":
				parts = append(parts, map[string]any{"type": "text", "text": responseText(firstNonNil(part["text"], part["content"]))})
			case "input_image", "image_url":
				imageURL := firstNonEmpty(stringFromAny(part["image_url"]), stringFromAny(part["url"]))
				if imageURL != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
				}
			}
		}
		if len(parts) == 1 {
			if part, ok := parts[0].(map[string]any); ok && stringFromAny(part["type"]) == "text" {
				return stringFromAny(part["text"])
			}
		}
		return parts
	default:
		return responseText(value)
	}
}

func collapseSystemMessages(messages []any) []any {
	var systems []string
	var rest []any
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if stringFromAny(message["role"]) == "system" {
			if text := responseText(message["content"]); text != "" {
				systems = append(systems, text)
			}
			continue
		}
		rest = append(rest, raw)
	}
	if len(systems) > 0 {
		rest = append([]any{map[string]any{"role": "system", "content": strings.Join(systems, "\n\n")}}, rest...)
	}
	return rest
}

func responsesToolsToChat(raw any) ([]any, map[string]string) {
	items, _ := raw.([]any)
	tools := make([]any, 0, len(items))
	names := map[string]string{}
	for index, rawTool := range items {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typeName := stringFromAny(tool["type"])
		if typeName != "function" && typeName != "custom" {
			continue
		}
		name := flattenedToolName(tool)
		if name == "" {
			name = fmt.Sprintf("tool_%d", index)
		}
		names[name] = stringFromAny(tool["name"])
		parameters, _ := tool["parameters"].(map[string]any)
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if typeName == "custom" {
			parameters = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []any{"input"}}
		}
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": stringFromAny(tool["description"]), "parameters": parameters,
		}})
	}
	return tools, names
}

func responsesToolChoiceToChat(raw any, names map[string]string) any {
	switch value := raw.(type) {
	case string:
		switch value {
		case "auto", "none", "required":
			return value
		}
	case map[string]any:
		name := flattenedToolName(value)
		if name == "" {
			name = stringFromAny(value["name"])
		}
		if name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return nil
}

func applyChatReasoningOptions(result, body map[string]any, model string) {
	reasoning, _ := body["reasoning"].(map[string]any)
	effort := firstNonEmpty(stringFromAny(reasoning["effort"]), stringFromAny(body["reasoning_effort"]))
	if effort == "" {
		return
	}
	result["reasoning_effort"] = effort
	if strings.Contains(model, "deepseek") || strings.Contains(model, "qwen") {
		result["enable_thinking"] = effort != "none" && effort != "minimal"
	}
}

func chatCompletionToResponse(body map[string]any, original map[string]any) (map[string]any, error) {
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		return nil, errors.New("chat response missing choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, errors.New("chat response choice missing message")
	}
	responseID := responseIDFromChat(stringFromAny(body["id"]))
	output := make([]any, 0)
	if reasoning := reasoningText(message); reasoning != "" {
		output = append(output, reasoningOutputItem(responseID, reasoning))
	}
	if content := chatMessageOutputItem(responseID, message); content != nil {
		output = append(output, content)
	}
	toolContext := buildProtocolToolContext(original["tools"])
	output = append(output, chatToolCallOutputItems(message, toolContext)...)
	finishReason := stringFromAny(choice["finish_reason"])
	status := "completed"
	if finishReason == "length" {
		status = "incomplete"
	}
	response := map[string]any{
		"id": responseID, "object": "response", "created_at": uint64FromAny(body["created"], 0),
		"status": status, "model": stringFromAny(body["model"]), "output": output,
		"usage": chatUsageToResponses(body["usage"]),
	}
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	copyResponseRequestFields(response, original)
	return response, nil
}

func chatMessageOutputItem(responseID string, message map[string]any) any {
	var content []any
	switch raw := message["content"].(type) {
	case string:
		_, answer := splitLeadingThinkBlock(raw)
		if answer == "" && !strings.Contains(raw, "<think>") {
			answer = raw
		}
		if answer != "" {
			content = append(content, map[string]any{"type": "output_text", "text": answer, "annotations": []any{}})
		}
	case []any:
		for _, rawPart := range raw {
			part, _ := rawPart.(map[string]any)
			typeName := stringFromAny(part["type"])
			if typeName == "text" || typeName == "output_text" {
				content = append(content, map[string]any{"type": "output_text", "text": stringFromAny(part["text"]), "annotations": []any{}})
			}
		}
	}
	if refusal := stringFromAny(message["refusal"]); refusal != "" {
		content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
	}
	if len(content) == 0 {
		return nil
	}
	return map[string]any{"id": responseID + "_msg", "type": "message", "status": "completed", "role": "assistant", "content": content}
}

func chatToolCallOutputItems(message map[string]any, toolContext protocolToolContext) []any {
	var output []any
	if calls, ok := message["tool_calls"].([]any); ok {
		for index, raw := range calls {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			callID := firstString(stringFromAny(call["id"]), fmt.Sprintf("call_%d", index))
			output = append(output, protocolResponseToolCall(callID, stringFromAny(function["name"]), normalizeChatToolArguments(function["arguments"]), toolContext))
		}
	} else if function, ok := message["function_call"].(map[string]any); ok {
		callID := firstString(stringFromAny(function["id"]), "call_0")
		output = append(output, protocolResponseToolCall(callID, stringFromAny(function["name"]), normalizeChatToolArguments(function["arguments"]), toolContext))
	}
	return output
}

func reasoningOutputItem(responseID, text string) map[string]any {
	return map[string]any{"id": "rs_" + responseID, "type": "reasoning", "reasoning_content": text, "summary": []any{map[string]any{"type": "summary_text", "text": text}}}
}

func reasoningText(value map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if text, ok := value[key].(string); ok && text != "" {
			return text
		}
	}
	if reasoning, ok := value["reasoning"].(map[string]any); ok {
		for _, key := range []string{"content", "text", "summary"} {
			if text := responseText(reasoning[key]); text != "" {
				return text
			}
		}
	}
	if text := reasoningDetailsText(value["reasoning_details"]); text != "" {
		return text
	}
	if text, ok := value["content"].(string); ok {
		reasoning, _ := splitLeadingThinkBlock(text)
		return reasoning
	}
	return ""
}

func reasoningDetailsText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, part := range value {
			if text := reasoningDetailsText(part); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n\n")
	case map[string]any:
		for _, key := range []string{"text", "content", "summary"} {
			if text, ok := value[key].(string); ok && text != "" {
				return text
			}
		}
		return reasoningDetailsText(value["parts"])
	default:
		return ""
	}
}

func chatUsageToResponses(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	input := uint64FromAny(firstNonNil(usage["prompt_tokens"], usage["input_tokens"], usage["promptTokenCount"]), 0)
	inputIncludesCache := usage["prompt_tokens"] != nil
	output := uint64FromAny(firstNonNil(usage["completion_tokens"], usage["output_tokens"], usage["candidatesTokenCount"]), 0)
	cached := uint64(0)
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		cached = uint64FromAny(details["cached_tokens"], 0)
	}
	if cached == 0 {
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			cached = uint64FromAny(details["cached_tokens"], 0)
		}
	}
	if usage["cachedContentTokenCount"] != nil {
		cached = uint64FromAny(usage["cachedContentTokenCount"], 0)
	}
	cacheCreation := uint64FromAny(usage["cache_creation_input_tokens"], 0)
	cacheCreation5m := uint64FromAny(usage["cache_creation_5m_input_tokens"], 0)
	cacheCreation1h := uint64FromAny(usage["cache_creation_1h_input_tokens"], 0)
	cacheCreated := cacheCreation
	if cacheCreated == 0 {
		cacheCreated = cacheCreation5m + cacheCreation1h
	}
	hasClaudeCache := usage["cache_read_input_tokens"] != nil || usage["cache_creation_input_tokens"] != nil || usage["cache_creation_5m_input_tokens"] != nil || usage["cache_creation_1h_input_tokens"] != nil
	if usage["input_tokens"] != nil {
		input = uint64FromAny(usage["input_tokens"], 0)
		inputIncludesCache = false
	}
	if usage["cache_read_input_tokens"] != nil {
		cached = uint64FromAny(usage["cache_read_input_tokens"], 0)
	}
	if usage["promptTokenCount"] != nil {
		prompt := uint64FromAny(usage["promptTokenCount"], 0)
		if cached > prompt {
			cached = prompt
		}
		input = prompt - cached
		inputIncludesCache = false
	}
	usageInput := input
	if inputIncludesCache {
		deduct := cached + cacheCreated
		if deduct > usageInput {
			usageInput = 0
		} else {
			usageInput -= deduct
		}
	}
	total := uint64FromAny(usage["total_tokens"], usageInput+output)
	if usage["total_tokens"] == nil || cached > 0 || cacheCreated > 0 || usage["promptTokenCount"] != nil {
		total = usageInput + output + cached + cacheCreated
	}
	result := map[string]any{"input_tokens": usageInput, "output_tokens": output, "total_tokens": total}
	if cached > 0 && !hasClaudeCache {
		result["input_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if details, ok := usage["completion_tokens_details"]; ok {
		result["output_tokens_details"] = details
	}
	for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation_5m_input_tokens", "cache_creation_1h_input_tokens"} {
		if usage[key] != nil {
			result[key] = usage[key]
		}
	}
	if cacheCreation5m > 0 && cacheCreation1h > 0 {
		result["cache_ttl"] = "mixed"
	} else if cacheCreation5m > 0 {
		result["cache_ttl"] = "5m"
	} else if cacheCreation1h > 0 {
		result["cache_ttl"] = "1h"
	}
	return result
}

func responsesErrorFromUpstream(status int, contentType string, body []byte) map[string]any {
	errorObject := map[string]any{"message": fmt.Sprintf("Upstream returned HTTP %d", status), "type": "upstream_error", "code": strconv.Itoa(status)}
	if strings.Contains(strings.ToLower(contentType), "json") {
		var value map[string]any
		if json.Unmarshal(body, &value) == nil {
			source, _ := value["error"].(map[string]any)
			if source == nil {
				source = value
			}
			for _, key := range []string{"message", "type", "code", "param"} {
				if source[key] != nil {
					errorObject[key] = source[key]
				}
			}
		}
	} else if preview := strings.TrimSpace(string(body)); preview != "" {
		runes := []rune(preview)
		if len(runes) > 1024 {
			runes = runes[:1024]
		}
		errorObject["message"] = string(runes)
	}
	return map[string]any{"error": errorObject}
}

func writeProtocolProxyResponse(w http.ResponseWriter, resp *http.Response, ctx protocolProxyContext) (int64, error) {
	contentType := resp.Header.Get("content-type")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		if err != nil {
			return 0, err
		}
		payload, _ := json.Marshal(responsesErrorFromUpstream(resp.StatusCode, contentType, body))
		w.Header().Set("content-type", "application/json; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		n, err := w.Write(payload)
		return int64(n), err
	}
	if !ctx.Converted {
		if contentType != "" {
			w.Header().Set("content-type", contentType)
		}
		w.WriteHeader(resp.StatusCode)
		flushRelayResponseHeaders(w)
		return copyRelayResponseBody(w, resp.Body)
	}
	if ctx.Stream || strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		w.Header().Set("content-type", "text/event-stream; charset=utf-8")
		w.Header().Set("cache-control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flushRelayResponseHeaders(w)
		return streamChatSSEAsResponses(w, resp.Body, ctx.OriginalRequest)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return 0, err
	}
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return 0, fmt.Errorf("invalid Chat Completions response: %w", err)
	}
	converted, err := chatCompletionToResponse(chat, ctx.OriginalRequest)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(converted)
	if err != nil {
		return 0, err
	}
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	n, err := w.Write(payload)
	return int64(n), err
}

type chatSSEState struct {
	started        bool
	completed      bool
	responseID     string
	model          string
	createdAt      uint64
	text           strings.Builder
	reasoning      strings.Builder
	textAdded      bool
	reasoningAdded bool
	tools          map[int]*chatSSETool
	usage          map[string]any
	finishReason   string
	request        map[string]any
	toolContext    protocolToolContext
}

type chatSSETool struct {
	Index     int
	Output    int
	CallID    string
	Name      string
	Arguments strings.Builder
	Added     bool
}

func streamChatSSEAsResponses(w io.Writer, reader io.Reader, request map[string]any) (int64, error) {
	state := &chatSSEState{responseID: "resp_compat", tools: map[int]*chatSSETool{}, request: request, toolContext: buildProtocolToolContext(request["tools"])}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var block []string
	var written int64
	flush := func() error {
		if len(block) == 0 {
			return nil
		}
		chunk, done, err := state.consumeBlock(block)
		block = nil
		if err != nil {
			return err
		}
		if len(chunk) > 0 {
			n, err := io.WriteString(w, chunk)
			written += int64(n)
			if err != nil {
				return err
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if done {
			state.completed = true
		}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return written, err
			}
			continue
		}
		block = append(block, line)
	}
	if err := scanner.Err(); err != nil {
		return written, err
	}
	if err := flush(); err != nil {
		return written, err
	}
	if !state.completed {
		chunk := state.finishEvents()
		n, err := io.WriteString(w, chunk)
		written += int64(n)
		return written, err
	}
	return written, nil
}

func (s *chatSSEState) consumeBlock(lines []string) (string, bool, error) {
	var event string
	var data []string
	for _, line := range lines {
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(value)
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(value))
		}
	}
	if len(data) == 0 {
		return "", false, nil
	}
	joined := strings.Join(data, "\n")
	if joined == "[DONE]" {
		return s.finishEvents(), true, nil
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(joined), &chunk); err != nil {
		return "", false, nil
	}
	if event == "error" || chunk["error"] != nil {
		message := "upstream stream error"
		if source, ok := chunk["error"].(map[string]any); ok {
			message = firstString(stringFromAny(source["message"]), stringFromAny(source["detail"]), message)
		} else if text := stringFromAny(chunk["error"]); text != "" {
			message = text
		}
		return s.failedEvents(message), true, nil
	}
	return s.chunkEvents(chunk), false, nil
}

func (s *chatSSEState) chunkEvents(chunk map[string]any) string {
	if id := stringFromAny(chunk["id"]); id != "" {
		s.responseID = responseIDFromChat(id)
	}
	if model := stringFromAny(chunk["model"]); model != "" {
		s.model = model
	}
	s.createdAt = uint64FromAny(chunk["created"], s.createdAt)
	var out strings.Builder
	s.ensureStarted(&out)
	if usage := chatUsageToResponses(chunk["usage"]); uint64FromAny(usage["total_tokens"], 0) > 0 {
		s.usage = usage
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return out.String()
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if reasoning := reasoningText(delta); reasoning != "" {
		s.addReasoningDelta(&out, reasoning)
	}
	if content := stringFromAny(delta["content"]); content != "" {
		s.addTextDelta(&out, content)
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			s.addToolDelta(&out, raw)
		}
	}
	if finish := stringFromAny(choice["finish_reason"]); finish != "" {
		s.finishReason = finish
	}
	return out.String()
}

func (s *chatSSEState) ensureStarted(out *strings.Builder) {
	if s.started {
		return
	}
	s.started = true
	base := s.baseResponse("in_progress", []any{})
	writeSSE(out, "response.created", map[string]any{"type": "response.created", "response": base})
	writeSSE(out, "response.in_progress", map[string]any{"type": "response.in_progress", "response": base})
}

func (s *chatSSEState) addReasoningDelta(out *strings.Builder, delta string) {
	if !s.reasoningAdded {
		s.reasoningAdded = true
		writeSSE(out, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "rs_" + s.responseID, "type": "reasoning", "status": "in_progress", "summary": []any{}}})
		writeSSE(out, "response.reasoning_summary_part.added", map[string]any{"type": "response.reasoning_summary_part.added", "item_id": "rs_" + s.responseID, "output_index": 0, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
	}
	s.reasoning.WriteString(delta)
	writeSSE(out, "response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": "rs_" + s.responseID, "output_index": 0, "summary_index": 0, "delta": delta})
}

func (s *chatSSEState) addTextDelta(out *strings.Builder, delta string) {
	index := 0
	if s.reasoningAdded {
		index = 1
	}
	if !s.textAdded {
		s.textAdded = true
		writeSSE(out, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"id": s.responseID + "_msg", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}})
		writeSSE(out, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": s.responseID + "_msg", "output_index": index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	}
	s.text.WriteString(delta)
	writeSSE(out, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": s.responseID + "_msg", "output_index": index, "content_index": 0, "delta": delta})
}

func (s *chatSSEState) addToolDelta(out *strings.Builder, raw any) {
	call, _ := raw.(map[string]any)
	index := int(uint64FromAny(call["index"], 0))
	tool := s.tools[index]
	if tool == nil {
		tool = &chatSSETool{Index: index}
		s.tools[index] = tool
	}
	if id := stringFromAny(call["id"]); id != "" {
		tool.CallID = id
	}
	function, _ := call["function"].(map[string]any)
	if name := stringFromAny(function["name"]); name != "" {
		tool.Name = name
	}
	args := stringFromAny(function["arguments"])
	if !tool.Added && (tool.CallID != "" || tool.Name != "") {
		tool.Added = true
		tool.Output = s.nextToolOutputIndex(index)
		if tool.CallID == "" {
			tool.CallID = fmt.Sprintf("call_%d", index)
		}
		item := map[string]any{"id": "fc_" + tool.CallID, "type": "function_call", "status": "in_progress", "call_id": tool.CallID, "name": tool.Name, "arguments": ""}
		if spec, ok := s.toolContext.Custom[tool.Name]; ok {
			item = map[string]any{"id": "ctc_" + tool.CallID, "type": "custom_tool_call", "status": "in_progress", "call_id": tool.CallID, "name": spec.OpenAIName, "input": ""}
		} else if spec, ok := s.toolContext.Function[tool.Name]; ok {
			item["name"] = spec.Name
			if spec.Namespace != "" {
				item["namespace"] = spec.Namespace
			}
		}
		writeSSE(out, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": tool.Output, "item": item})
	}
	if args != "" {
		tool.Arguments.WriteString(args)
		if _, custom := s.toolContext.Custom[tool.Name]; !custom {
			writeSSE(out, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_" + tool.CallID, "output_index": tool.Output, "delta": args})
		}
	}
}

func (s *chatSSEState) nextToolOutputIndex(index int) int {
	base := 0
	if s.reasoningAdded {
		base++
	}
	if s.textAdded {
		base++
	}
	return base + index
}

func (s *chatSSEState) finishEvents() string {
	if s.completed {
		return ""
	}
	var out strings.Builder
	s.ensureStarted(&out)
	var output []any
	if s.reasoningAdded {
		item := reasoningOutputItem(s.responseID, s.reasoning.String())
		output = append(output, item)
		writeSSE(&out, "response.reasoning_summary_text.done", map[string]any{"type": "response.reasoning_summary_text.done", "item_id": "rs_" + s.responseID, "output_index": 0, "summary_index": 0, "text": s.reasoning.String()})
		writeSSE(&out, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	}
	if s.textAdded {
		index := 0
		if s.reasoningAdded {
			index = 1
		}
		item := map[string]any{"id": s.responseID + "_msg", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}}}
		output = append(output, item)
		writeSSE(&out, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": s.responseID + "_msg", "output_index": index, "content_index": 0, "text": s.text.String()})
		writeSSE(&out, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	}
	keys := make([]int, 0, len(s.tools))
	for key := range s.tools {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	for _, key := range keys {
		tool := s.tools[key]
		item := protocolResponseToolCall(tool.CallID, tool.Name, tool.Arguments.String(), s.toolContext)
		output = append(output, item)
		if stringFromAny(item["type"]) == "custom_tool_call" {
			writeSSE(&out, "response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "item_id": stringFromAny(item["id"]), "call_id": tool.CallID, "output_index": tool.Output, "delta": stringFromAny(item["input"])})
		} else {
			writeSSE(&out, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_" + tool.CallID, "output_index": tool.Output, "arguments": stringFromAny(item["arguments"])})
		}
		writeSSE(&out, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tool.Output, "item": item})
	}
	status := "completed"
	if s.finishReason == "length" {
		status = "incomplete"
	}
	response := s.baseResponse(status, output)
	copyResponseRequestFields(response, s.request)
	writeSSE(&out, "response.completed", map[string]any{"type": "response.completed", "response": response})
	out.WriteString("data: [DONE]\n\n")
	s.completed = true
	return out.String()
}

func (s *chatSSEState) failedEvents(message string) string {
	var out strings.Builder
	s.ensureStarted(&out)
	writeSSE(&out, "response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": s.responseID, "object": "response", "status": "failed", "error": map[string]any{"message": message, "type": "upstream_error"}}})
	s.completed = true
	return out.String()
}

func (s *chatSSEState) baseResponse(status string, output []any) map[string]any {
	usage := s.usage
	if usage == nil {
		usage = chatUsageToResponses(nil)
	}
	return map[string]any{"id": s.responseID, "object": "response", "created_at": s.createdAt, "status": status, "model": s.model, "output": output, "usage": usage}
}

func writeSSE(out *strings.Builder, event string, data any) {
	payload, _ := json.Marshal(data)
	out.WriteString("event: ")
	out.WriteString(event)
	out.WriteString("\ndata: ")
	out.Write(payload)
	out.WriteString("\n\n")
}

func responseIDFromChat(id string) string {
	if id == "" {
		id = "compat"
	}
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + id
}

func splitLeadingThinkBlock(text string) (string, string) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<think>") {
		return "", ""
	}
	closeIndex := strings.Index(trimmed, "</think>")
	if closeIndex < 0 {
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "<think>")), ""
	}
	reasoning := strings.TrimSpace(trimmed[len("<think>"):closeIndex])
	answer := strings.TrimSpace(trimmed[closeIndex+len("</think>"):])
	return reasoning, answer
}

func responseText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case nil:
		return ""
	default:
		return canonicalJSONString(value)
	}
}

func canonicalJSONString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func flattenedToolName(value map[string]any) string {
	name := stringFromAny(value["name"])
	namespace := stringFromAny(value["namespace"])
	if namespace == "" || name == "" {
		return name
	}
	return namespace + "__" + name
}

func copyResponseRequestFields(target, source map[string]any) {
	for _, key := range []string{"background", "max_output_tokens", "metadata", "parallel_tool_calls", "previous_response_id", "service_tier", "temperature", "tool_choice", "tools", "top_p", "truncation"} {
		copyIfPresent(target, source, key)
	}
}

func copyIfPresent(target, source map[string]any, key string) {
	if value, ok := source[key]; ok {
		target[key] = value
	}
}

func cloneMap(value map[string]any) map[string]any {
	result := map[string]any{}
	for key, item := range value {
		result[key] = item
	}
	return result
}
