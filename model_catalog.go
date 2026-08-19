package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	modelBaseURLEnvKeys = []string{
		"CODEX_PLUS_OPENAI_BASE_URL",
		"CODEX_PLUS_BASE_URL",
		"OPENAI_BASE_URL",
		"OPENAI_API_BASE_URL",
		"OPENAI_API_BASE",
		"OPENAI_API_URL",
	}
	modelAPIKeyEnvKeys = []string{
		"CODEX_PLUS_OPENAI_API_KEY",
		"CODEX_PLUS_API_KEY",
		"OPENAI_API_KEY",
	}
)

type codexModelSource struct {
	ID           string
	Type         string
	Name         string
	BaseURL      string
	APIKey       string
	ProxyEnabled bool
	ProxyURL     string
}

func codexModelCatalogValue() map[string]any {
	settings := loadSettings()
	if len(settings.RelayProfiles) > 0 && settings.RelayProfilesEnabled {
		profile := activeRelayProfile(settings)
		if strings.TrimSpace(profile.ModelList) != "" || strings.TrimSpace(profile.Model) != "" {
			return relayProfileModelCatalogValue(profile)
		}
		if profile.RelayMode == "aggregate" {
			if member, err := selectRelayForProbe(settings); err == nil && effectiveUpstreamBaseURL(member) != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				return relayProfileRemoteModelCatalogValue(ctx, member)
			}
		}
		if profile.RelayMode != "official" && profile.RelayMode != "aggregate" && effectiveUpstreamBaseURL(profile) != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			return relayProfileRemoteModelCatalogValue(ctx, profile)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return codexModelCatalogFromHome(ctx, codexHomeDir())
}

func relayProfileModelCatalogValue(profile relayProfile) map[string]any {
	entries := collectModelCatalogEntries(profile.ModelList, profile.ModelWindows, profile.Model)
	models := make([]string, 0, len(entries))
	for _, entry := range entries {
		models = append(models, entry.Slug)
	}
	model := strings.TrimSpace(profile.Model)
	if slug, _, ok := parseModelSuffix(model); ok {
		model = slug
	}
	defaultModel := ""
	if containsString(models, model) {
		defaultModel = model
	} else if len(models) > 0 {
		defaultModel = models[0]
	}
	providerName := strings.TrimSpace(profile.Name)
	if providerName == "" {
		providerName = strings.TrimSpace(profile.ID)
	}
	status := "ok"
	if len(models) == 0 {
		status = "not_configured"
	}
	return map[string]any{
		"status":               status,
		"path":                 filepath.Join(codexHomeDir(), "config.toml"),
		"service_tier":         codexConfigServiceTier(codexHomeDir()),
		"model":                model,
		"default_model":        defaultModel,
		"model_provider":       strings.TrimSpace(profile.ID),
		"codex_model_provider": codexModelProviderForRelayProfile(codexHomeDir(), profile),
		"provider_name":        providerName,
		"models":               models,
		"modelMetadata":        modelMetadataForNames(models),
		"sources": []any{map[string]any{
			"id":            "relay-profile:" + strings.TrimSpace(profile.ID),
			"type":          "relay_profile_model_list",
			"name":          providerName,
			"base_url":      safeStatusURL(firstNonEmpty(profile.UpstreamBaseURL, profile.BaseURL)),
			"status":        status,
			"models":        len(models),
			"responses_api": responsesAPIStatus("unknown", "", ""),
		}},
		"responses_api": responsesAPIStatus("unknown", "", ""),
	}
}

func relayProfileRemoteModelCatalogValue(ctx context.Context, profile relayProfile) map[string]any {
	source := codexModelSource{
		ID:           "relay-profile:" + strings.TrimSpace(profile.ID),
		Type:         "relay_profile",
		Name:         displayRelayName(profile),
		BaseURL:      effectiveUpstreamBaseURL(profile),
		APIKey:       strings.TrimSpace(profile.APIKey),
		ProxyEnabled: profile.ProxyEnabled,
		ProxyURL:     profile.ProxyURL,
	}
	models, sourceStatus := fetchModelsFromSource(ctx, source)
	model := strings.TrimSpace(profile.Model)
	if slug, _, ok := parseModelSuffix(model); ok {
		model = slug
	}
	defaultModel := ""
	if containsString(models, model) {
		defaultModel = model
	} else if len(models) > 0 {
		defaultModel = models[0]
	}
	status := "not_configured"
	if len(models) > 0 {
		status = "ok"
	} else if stringFromAny(sourceStatus["status"]) == "failed" {
		status = "failed"
	}
	return map[string]any{
		"status":               status,
		"path":                 filepath.Join(codexHomeDir(), "config.toml"),
		"service_tier":         codexConfigServiceTier(codexHomeDir()),
		"model":                model,
		"default_model":        defaultModel,
		"model_provider":       strings.TrimSpace(profile.ID),
		"codex_model_provider": codexModelProviderForRelayProfile(codexHomeDir(), profile),
		"provider_name":        displayRelayName(profile),
		"models":               models,
		"modelMetadata":        modelMetadataForNames(models),
		"sources":              []any{sourceStatus},
		"responses_api":        responsesAPIStatus("unknown", "", ""),
	}
}

func codexModelProviderForRelayProfile(home string, profile relayProfile) string {
	if provider := strings.TrimSpace(rootKeyString(profile.ConfigContents, "model_provider")); provider != "" {
		return provider
	}
	return strings.TrimSpace(rootKeyString(readFile(filepath.Join(home, "config.toml")), "model_provider"))
}

func codexModelCatalogFromHome(ctx context.Context, home string) map[string]any {
	configPath := filepath.Join(home, "config.toml")
	contents, err := os.ReadFile(configPath)
	configText := string(contents)
	configMissing := false
	if err != nil {
		if os.IsNotExist(err) {
			configMissing = true
		} else {
			return map[string]any{
				"status":         "failed",
				"path":           configPath,
				"message":        err.Error(),
				"service_tier":   nil,
				"model":          "",
				"model_provider": "",
				"provider_name":  "",
				"default_model":  "",
				"models":         []string{},
				"sources":        []any{},
				"responses_api":  responsesAPIStatus("unknown", "", ""),
			}
		}
	}
	effective := effectiveCodexRootValues(configText)
	model := strings.TrimSpace(effective["model"])
	modelProvider := strings.TrimSpace(effective["model_provider"])
	resolvedProvider, providerValues := codexProviderValues(configText, modelProvider)
	if modelProvider == "" && resolvedProvider != "" {
		modelProvider = resolvedProvider
	}
	providerName := strings.TrimSpace(unquoteToml(providerValues["name"]))
	if providerName == "" {
		providerName = modelProvider
	}
	authAPIKey := readCodexAuthAPIKey(filepath.Join(home, "auth.json"))
	sources := modelSourcesFromEnvironment(authAPIKey)
	if !configMissing {
		if source, ok := modelSourceFromConfig(configText, effective, providerValues, authAPIKey); ok {
			if !modelSourcesContainBaseURL(sources, source.BaseURL) {
				sources = append(sources, source)
			}
		}
	}
	var sourceStatuses []any
	var models []string
	for _, source := range sources {
		sourceModels, status := fetchModelsFromSource(ctx, source)
		models = append(models, sourceModels...)
		sourceStatuses = append(sourceStatuses, status)
	}
	catalogModels, catalogStatus := modelsFromConfigModelCatalogJSON(home, effective["model_catalog_json"])
	models = append(models, catalogModels...)
	if catalogStatus != nil {
		sourceStatuses = append(sourceStatuses, catalogStatus)
	}
	models = uniqueStrings(models)
	if model == "" {
		model = strings.TrimSpace(effective["default_model"])
	}
	defaultModel := ""
	if containsString(models, model) {
		defaultModel = model
	} else if len(models) > 0 {
		defaultModel = models[0]
	}
	status := "not_configured"
	if len(models) > 0 {
		status = "ok"
	} else if anyFailedModelSource(sourceStatuses) {
		status = "failed"
	} else if configMissing {
		status = "missing"
	}
	return map[string]any{
		"status":         status,
		"path":           configPath,
		"service_tier":   nullableString(strings.TrimSpace(effective["service_tier"])),
		"model":          model,
		"default_model":  defaultModel,
		"model_provider": modelProvider,
		"provider_name":  providerName,
		"models":         models,
		"modelMetadata":  modelMetadataForNames(models),
		"sources":        sourceStatuses,
		"responses_api":  preferredResponsesAPIStatus(sourceStatuses),
	}
}

func codexConfigServiceTier(home string) any {
	contents, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return nil
	}
	return nullableString(strings.TrimSpace(effectiveCodexRootValues(string(contents))["service_tier"]))
}

func splitModelList(value string) []string {
	return strings.FieldsFunc(value, func(ch rune) bool {
		return ch == '\n' || ch == '\r' || ch == ','
	})
}

func effectiveCodexRootValues(contents string) map[string]string {
	effective := rootValues(contents)
	if profile := strings.TrimSpace(effective["profile"]); profile != "" {
		for key, value := range tableValues(contents, "profiles."+profile) {
			effective[key] = unquoteToml(value)
		}
	}
	return effective
}

func rootValues(contents string) map[string]string {
	values := map[string]string{}
	for _, line := range splitLines(contents) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			break
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, raw, ok := strings.Cut(trimmed, "=")
		if ok {
			values[strings.TrimSpace(key)] = unquoteToml(raw)
		}
	}
	return values
}

func codexProviderValues(contents, modelProvider string) (string, map[string]string) {
	if strings.TrimSpace(modelProvider) != "" {
		return modelProvider, tableValues(contents, "model_providers."+modelProvider)
	}
	providers := modelProviderNames(contents)
	if len(providers) == 1 {
		return providers[0], tableValues(contents, "model_providers."+providers[0])
	}
	return "", map[string]string{}
}

func modelProviderNames(contents string) []string {
	var names []string
	for _, line := range splitLines(contents) {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[model_providers.") || !strings.HasSuffix(trimmed, "]") {
			continue
		}
		table := strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
		parts := splitTomlTablePath(table)
		if len(parts) == 2 && parts[0] == "model_providers" {
			names = append(names, unquoteToml(parts[1]))
		}
	}
	return uniqueStrings(names)
}

func readCodexAuthAPIKey(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}
	for _, key := range []string{"OPENAI_API_KEY", "api_key", "apikey", "access_token", "token"} {
		if value := strings.TrimSpace(stringFromAny(payload[key])); value != "" {
			return value
		}
	}
	return ""
}

func modelSourcesFromEnvironment(authAPIKey string) []codexModelSource {
	baseURL := firstEnvValue(modelBaseURLEnvKeys)
	if baseURL == "" {
		return nil
	}
	apiKey := firstNonEmpty(firstEnvValue(modelAPIKeyEnvKeys), authAPIKey)
	return []codexModelSource{{
		ID:      "env:openai-compatible",
		Type:    "environment",
		Name:    "Environment",
		BaseURL: baseURL,
		APIKey:  apiKey,
	}}
}

func modelSourceFromConfig(contents string, effective map[string]string, providerValues map[string]string, authAPIKey string) (codexModelSource, bool) {
	baseURL := strings.TrimSpace(unquoteToml(providerValues["base_url"]))
	if baseURL == "" {
		return codexModelSource{}, false
	}
	providerName := strings.TrimSpace(unquoteToml(providerValues["name"]))
	modelProvider := strings.TrimSpace(effective["model_provider"])
	if providerName == "" {
		providerName = modelProvider
	}
	return codexModelSource{
		ID:      "config:" + firstNonEmpty(modelProvider, providerName),
		Type:    "config",
		Name:    providerName,
		BaseURL: baseURL,
		APIKey:  providerAPIKey(providerValues, authAPIKey),
	}, true
}

func providerAPIKey(providerValues map[string]string, authAPIKey string) string {
	for _, key := range []string{"experimental_bearer_token", "api_key", "apikey", "bearer_token", "token"} {
		if value := strings.TrimSpace(unquoteToml(providerValues[key])); value != "" {
			return value
		}
	}
	for _, key := range []string{"env_key", "api_key_env", "api_key_env_var", "key_env", "bearer_token_env"} {
		if envName := strings.TrimSpace(unquoteToml(providerValues[key])); envName != "" {
			if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
				return value
			}
		}
	}
	return firstNonEmpty(firstEnvValue(modelAPIKeyEnvKeys), authAPIKey)
}

func firstEnvValue(names []string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func modelSourcesContainBaseURL(sources []codexModelSource, baseURL string) bool {
	target := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, source := range sources {
		if strings.TrimRight(strings.TrimSpace(source.BaseURL), "/") == target {
			return true
		}
	}
	return false
}

func fetchModelsFromSource(ctx context.Context, source codexModelSource) ([]string, map[string]any) {
	endpoint := modelsEndpoint(source.BaseURL)
	status := map[string]any{
		"id":            source.ID,
		"type":          source.Type,
		"name":          source.Name,
		"base_url":      safeStatusURL(source.BaseURL),
		"endpoint":      safeStatusURL(endpoint),
		"auth":          map[bool]string{true: "present", false: "missing"}[strings.TrimSpace(source.APIKey) != ""],
		"responses_api": responsesAPIStatus("unknown", "", ""),
	}
	if endpoint == "" {
		status["status"] = "failed"
		status["message"] = "Missing base URL"
		status["models"] = 0
		return nil, status
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		status["status"] = "failed"
		status["message"] = err.Error()
		status["models"] = 0
		return nil, status
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "ChatGPTCodexTools-GoManager/"+version)
	if strings.TrimSpace(source.APIKey) != "" {
		req.Header.Set("authorization", "Bearer "+strings.TrimSpace(source.APIKey))
	}
	client, err := relayHTTPClient(relayProfile{ProxyEnabled: source.ProxyEnabled, ProxyURL: source.ProxyURL})
	if err != nil {
		status["status"] = "failed"
		status["message"] = err.Error()
		status["models"] = 0
		return nil, status
	}
	resp, err := client.Do(req)
	if err != nil {
		status["status"] = "failed"
		status["message"] = err.Error()
		status["models"] = 0
		return nil, status
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		status["status"] = "failed"
		status["message"] = fmt.Sprintf("HTTP %d", resp.StatusCode)
		status["models"] = 0
		return nil, status
	}
	var payload any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		status["status"] = "failed"
		status["message"] = err.Error()
		status["models"] = 0
		return nil, status
	}
	models := uniqueStrings(parseModelPayload(payload))
	status["status"] = "ok"
	status["models"] = len(models)
	return models, status
}

func modelsEndpoint(baseURL string) string {
	cleaned := strings.TrimRight(safeStatusURL(baseURL), "/")
	if cleaned == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(cleaned), "/chat/completions") {
		cleaned = cleaned[:len(cleaned)-len("/chat/completions")]
	}
	if strings.HasSuffix(cleaned, "/models") {
		return cleaned
	}
	if hasVersionSuffix(cleaned) {
		return cleaned + "/models"
	}
	return cleaned + "/v1/models"
}

func hasVersionSuffix(baseURL string) bool {
	segment := baseURL
	if index := strings.LastIndex(segment, "/"); index >= 0 {
		segment = segment[index+1:]
	}
	if len(segment) < 2 || segment[0] != 'v' {
		return false
	}
	return segment[1] >= '0' && segment[1] <= '9'
}

func safeStatusURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.Split(strings.Split(rawURL, "?")[0], "#")[0]
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func parseModelPayload(payload any) []string {
	switch value := payload.(type) {
	case []any:
		var models []string
		for _, item := range value {
			models = append(models, parseModelPayloadItem(item)...)
		}
		return models
	case map[string]any:
		for _, key := range []string{"data", "models", "items"} {
			if nested, ok := value[key]; ok {
				if models := parseModelPayload(nested); len(models) > 0 {
					return models
				}
			}
		}
		return parseModelPayloadItem(value)
	default:
		return parseModelPayloadItem(payload)
	}
}

func parseModelPayloadItem(item any) []string {
	switch value := item.(type) {
	case string:
		return []string{strings.TrimSpace(value)}
	case map[string]any:
		for _, key := range []string{"id", "model", "name"} {
			if model := strings.TrimSpace(stringFromAny(value[key])); model != "" {
				return []string{model}
			}
		}
	}
	return nil
}

func modelsFromConfigModelCatalogJSON(home, rawPath string) ([]string, map[string]any) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return nil, nil
	}
	path := rawPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(home, path)
	}
	status := map[string]any{
		"id":            "config:model_catalog_json",
		"type":          "model_catalog_json",
		"name":          "Codex model catalog",
		"path":          path,
		"responses_api": responsesAPIStatus("unknown", "", ""),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		status["status"] = "failed"
		status["message"] = err.Error()
		status["models"] = 0
		return nil, status
	}
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		status["status"] = "failed"
		status["message"] = err.Error()
		status["models"] = 0
		return nil, status
	}
	models := uniqueStrings(parseModelCatalogJSONModels(payload))
	status["status"] = "ok"
	status["models"] = len(models)
	return models, status
}

func parseModelCatalogJSONModels(payload any) []string {
	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	items, ok := root["models"].([]any)
	if !ok {
		return nil
	}
	var models []string
	for _, item := range items {
		model, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if supported, ok := model["supported_in_api"].(bool); ok && !supported {
			continue
		}
		visibility := strings.TrimSpace(stringFromAny(model["visibility"]))
		if visibility != "" && !strings.EqualFold(visibility, "list") {
			continue
		}
		if slug := strings.TrimSpace(stringFromAny(model["slug"])); slug != "" {
			models = append(models, slug)
		}
	}
	return models
}

func responsesAPIStatus(status, endpoint, message string) map[string]any {
	return map[string]any{"status": status, "endpoint": endpoint, "message": message}
}

func preferredResponsesAPIStatus(sources []any) map[string]any {
	for _, wanted := range []string{"unsupported", "supported", "failed"} {
		for _, source := range sources {
			sourceMap, _ := source.(map[string]any)
			responses, _ := sourceMap["responses_api"].(map[string]any)
			if stringFromAny(responses["status"]) == wanted {
				return responses
			}
		}
	}
	return responsesAPIStatus("unknown", "", "")
}

func anyFailedModelSource(sources []any) bool {
	for _, source := range sources {
		sourceMap, _ := source.(map[string]any)
		if stringFromAny(sourceMap["status"]) == "failed" {
			return true
		}
	}
	return false
}
