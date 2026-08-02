package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const stepwiseMaxPromptLength = 420

type stepwiseRequest struct {
	LastUserMessage      string `json:"lastUserMessage"`
	LastAssistantMessage string `json:"lastAssistantMessage"`
	ThreadTitle          string `json:"threadTitle"`
	PageURL              string `json:"pageUrl"`
}

type stepwiseItem struct {
	Label  string `json:"label,omitempty"`
	Prompt string `json:"prompt"`
}

func stepwisePublicSettingsValue(settings backendSettings) map[string]any {
	envName := strings.TrimSpace(settings.CodexAppStepwiseAPIKeyEnv)
	return map[string]any{
		"status": "ok",
		"settings": map[string]any{
			"enabled":             settings.CodexAppStepwiseEnabled,
			"directSend":          settings.CodexAppStepwiseDirectSend,
			"baseUrlConfigured":   strings.TrimSpace(settings.CodexAppStepwiseBaseURL) != "",
			"apiKeyConfigured":    stepwiseAPIKey(settings) != "",
			"apiKeyEnv":           envName,
			"apiKeyEnvConfigured": envName != "" && strings.TrimSpace(os.Getenv(envName)) != "",
			"model":               settings.CodexAppStepwiseModel,
			"maxItems":            settings.CodexAppStepwiseMaxItems,
			"maxInputChars":       settings.CodexAppStepwiseMaxInputChars,
			"maxOutputTokens":     settings.CodexAppStepwiseMaxOutputTokens,
			"timeoutMs":           settings.CodexAppStepwiseTimeoutMS,
		},
	}
}

func stepwiseGenerateValue(payload map[string]any, settings backendSettings) map[string]any {
	requestValue := payload["request"]
	if requestValue == nil {
		requestValue = payload
	}
	var request stepwiseRequest
	_ = remarshal(requestValue, &request)
	return generateStepwise(context.Background(), request, settings)
}

func stepwiseTestValue(payload map[string]any, settings backendSettings) map[string]any {
	settings = stepwiseSettingsWithPayload(settings, payload)
	return generateStepwise(context.Background(), stepwiseRequest{
		LastUserMessage:      "测试 Stepwise 配置。",
		LastAssistantMessage: "Stepwise 应返回 0 到 6 条可直接发送的后续建议。",
		ThreadTitle:          "CodexTools Stepwise test",
	}, settings)
}

func (s *server) testStepwiseSettings(args map[string]any) commandResult {
	result := stepwiseTestValue(args, loadSettings())
	items, _ := result["items"].([]stepwiseItem)
	if stringFromAny(result["status"]) != "ok" {
		message := firstNonEmpty(stringFromAny(result["error"]), stringFromAny(result["message"]), "Stepwise 测试失败")
		return failed(message, result)
	}
	return ok(fmt.Sprintf("Stepwise 连接成功，返回 %d 条建议。", len(items)), result)
}

func stepwiseSettingsWithPayload(settings backendSettings, payload map[string]any) backendSettings {
	raw := payload["settings"]
	if raw == nil {
		return settings
	}
	var patch map[string]any
	if remarshal(raw, &patch) != nil {
		return settings
	}
	applyBool := func(key string, target *bool) {
		if _, ok := patch[key]; ok {
			*target = boolFromAny(patch[key])
		}
	}
	applyString := func(key string, target *string) {
		if _, ok := patch[key]; ok {
			*target = strings.TrimSpace(stringFromAny(patch[key]))
		}
	}
	applyBool("codexAppStepwiseEnabled", &settings.CodexAppStepwiseEnabled)
	applyBool("codexAppStepwiseDirectSend", &settings.CodexAppStepwiseDirectSend)
	applyString("codexAppStepwiseBaseUrl", &settings.CodexAppStepwiseBaseURL)
	applyString("codexAppStepwiseApiKey", &settings.CodexAppStepwiseAPIKey)
	applyString("codexAppStepwiseApiKeyEnv", &settings.CodexAppStepwiseAPIKeyEnv)
	applyString("codexAppStepwiseModel", &settings.CodexAppStepwiseModel)
	settings.CodexAppStepwiseMaxItems = intArg(patch, "codexAppStepwiseMaxItems", settings.CodexAppStepwiseMaxItems)
	settings.CodexAppStepwiseMaxInputChars = intArg(patch, "codexAppStepwiseMaxInputChars", settings.CodexAppStepwiseMaxInputChars)
	settings.CodexAppStepwiseMaxOutputTokens = intArg(patch, "codexAppStepwiseMaxOutputTokens", settings.CodexAppStepwiseMaxOutputTokens)
	settings.CodexAppStepwiseTimeoutMS = intArg(patch, "codexAppStepwiseTimeoutMs", settings.CodexAppStepwiseTimeoutMS)
	return normalizeSettings(settings)
}

func generateStepwise(parent context.Context, request stepwiseRequest, settings backendSettings) map[string]any {
	if !settings.CodexAppStepwiseEnabled {
		return map[string]any{"status": "ok", "disabled": true, "items": []stepwiseItem{}}
	}
	if settings.CodexAppStepwiseMaxItems == 0 {
		return map[string]any{"status": "ok", "items": []stepwiseItem{}}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(settings.CodexAppStepwiseBaseURL), "/")
	model := strings.TrimSpace(settings.CodexAppStepwiseModel)
	apiKey := stepwiseAPIKey(settings)
	if baseURL == "" || model == "" {
		return stepwiseFailed("Stepwise Base URL or Model is not configured")
	}
	if apiKey == "" {
		return stepwiseFailed("Stepwise API Key is not configured")
	}
	body, err := json.Marshal(map[string]any{
		"model":           model,
		"messages":        buildStepwiseMessages(request, settings),
		"temperature":     0.2,
		"max_tokens":      settings.CodexAppStepwiseMaxOutputTokens,
		"response_format": map[string]any{"type": "json_object"},
	})
	if err != nil {
		return stepwiseFailed(err.Error())
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(settings.CodexAppStepwiseTimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return stepwiseFailed(err.Error())
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+apiKey)
	req.Header.Set("user-agent", "CodexTools-Stepwise/"+version)
	client, err := relayHTTPClient(relayProfile{})
	if err != nil {
		return stepwiseFailed(err.Error())
	}
	resp, err := client.Do(req)
	if err != nil {
		appendDiagnosticLog("stepwise.request_failed", map[string]any{"error": err.Error(), "base_url": safeStatusURL(baseURL)})
		return stepwiseFailed("failed to request Stepwise API: " + err.Error())
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return stepwiseFailed(err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview := stepwiseShortText(string(responseBody), 240)
		return stepwiseFailed(fmt.Sprintf("Stepwise upstream %d: %s", resp.StatusCode, preview))
	}
	var data any
	if err := json.Unmarshal(responseBody, &data); err != nil {
		return stepwiseFailed("failed to parse Stepwise API response: " + err.Error())
	}
	items := extractStepwiseItems(data, settings.CodexAppStepwiseMaxItems)
	appendDiagnosticLog("stepwise.request_ok", map[string]any{"items": len(items), "model": model})
	return map[string]any{"status": "ok", "items": items}
}

func stepwiseFailed(message string) map[string]any {
	return map[string]any{"status": "failed", "message": message, "error": message, "items": []stepwiseItem{}}
}

func stepwiseAPIKey(settings backendSettings) string {
	if value := strings.TrimSpace(settings.CodexAppStepwiseAPIKey); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(strings.TrimSpace(settings.CodexAppStepwiseAPIKeyEnv)))
}

func buildStepwiseMessages(request stepwiseRequest, settings backendSettings) []any {
	limit := settings.CodexAppStepwiseMaxInputChars
	lastUser := stepwiseShortText(request.LastUserMessage, limit*35/100)
	lastAssistant := stepwiseShortText(request.LastAssistantMessage, limit*60/100)
	languageInput := lastUser
	if strings.TrimSpace(languageInput) == "" {
		languageInput = lastAssistant
	}
	system := strings.Join([]string{
		"You generate concise Codex Stepwise actions.",
		"Return strict JSON only, no markdown.",
		`Schema: {"items":[{"prompt":"...","label":"optional short label"}]}`,
		fmt.Sprintf("Generate 1 to %d items when the assistant result is non-empty.", settings.CodexAppStepwiseMaxItems),
		"Every prompt must be directly sendable by the user.",
		"Use the latest user intent and assistant result. Avoid generic filler.",
		"Language policy: write Stepwise prompts in the dominant natural language of languageInput.",
		"Ignore technical terms, file names, commands, APIs, and product names when detecting language; keep them in their original language when natural.",
		`Return {"items":[]} only when both the user intent and assistant result are empty or unusable.`,
	}, "\n")
	user, _ := json.Marshal(map[string]any{
		"lastUserMessage":      lastUser,
		"lastAssistantMessage": lastAssistant,
		"languageInput":        languageInput,
		"threadTitle":          stepwiseShortText(request.ThreadTitle, 240),
		"pageUrl":              stepwiseShortText(request.PageURL, 240),
		"maxItems":             settings.CodexAppStepwiseMaxItems,
	})
	return []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": string(user)},
	}
}

func extractStepwiseItems(data any, maxItems int) []stepwiseItem {
	for _, candidate := range stepwisePayloadCandidates(data) {
		if items := stepwiseItemsValue(candidate); items != nil {
			if result := clampStepwiseItems(items, maxItems); len(result) > 0 {
				return result
			}
		}
	}
	return []stepwiseItem{}
}

func stepwisePayloadCandidates(data any) []any {
	candidates := []any{data}
	object, _ := data.(map[string]any)
	if choices, ok := object["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if content := message["content"]; content != nil {
			candidates = append(candidates, content)
			if parsed := parseStepwiseJSONValue(content); parsed != nil {
				candidates = append(candidates, parsed)
			}
		}
	}
	for _, key := range []string{"output", "response", "data", "result"} {
		if value := object[key]; value != nil {
			candidates = append(candidates, value)
			if parsed := parseStepwiseJSONValue(value); parsed != nil {
				candidates = append(candidates, parsed)
			}
		}
	}
	return candidates
}

func stepwiseItemsValue(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	object, _ := value.(map[string]any)
	for _, key := range []string{"items", "suggestions", "next_steps", "nextSteps", "actions", "prompts"} {
		if items, ok := object[key].([]any); ok {
			return items
		}
	}
	return nil
}

func parseStepwiseJSONValue(value any) any {
	text := strings.TrimSpace(stringFromAny(value))
	if text == "" {
		return nil
	}
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return nil
	}
	return parsed
}

func clampStepwiseItems(raw []any, maxItems int) []stepwiseItem {
	seen := map[string]bool{}
	items := make([]stepwiseItem, 0, maxItems)
	for _, value := range raw {
		object, _ := value.(map[string]any)
		prompt := ""
		for _, key := range []string{"prompt", "text", "action", "content", "message"} {
			if prompt = strings.Join(strings.Fields(stringFromAny(object[key])), " "); prompt != "" {
				break
			}
		}
		if prompt == "" {
			prompt = strings.Join(strings.Fields(stringFromAny(value)), " ")
		}
		if prompt == "" || seen[prompt] {
			continue
		}
		seen[prompt] = true
		label := ""
		for _, key := range []string{"label", "title", "name"} {
			if label = strings.Join(strings.Fields(stringFromAny(object[key])), " "); label != "" {
				break
			}
		}
		items = append(items, stepwiseItem{Label: stepwiseShortText(label, 36), Prompt: stepwiseShortText(prompt, stepwiseMaxPromptLength)})
		if len(items) >= maxItems {
			break
		}
	}
	return items
}

func stepwiseShortText(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\u00a0", " "))
	for strings.Contains(value, "\n\n\n") {
		value = strings.ReplaceAll(value, "\n\n\n", "\n\n")
	}
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	return string(runes[len(runes)-limit:])
}
