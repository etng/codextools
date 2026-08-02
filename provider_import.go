package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func pendingProviderImportPath() string {
	return filepath.Join(stateDir(), "pending-provider-import.json")
}

func providerImportRequestFromURL(rawURL string) (providerImportRequest, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return providerImportRequest{}, err
	}
	values := parsed.Query()
	request := providerImportRequest{
		Name:      values.Get("name"),
		BaseURL:   values.Get("baseUrl"),
		APIKey:    values.Get("apiKey"),
		WireAPI:   firstNonEmpty(values.Get("wireApi"), "responses"),
		RelayMode: firstNonEmpty(values.Get("relayMode"), "pureApi"),
	}
	return normalizeProviderImportRequest(request)
}

func normalizeProviderImportRequest(request providerImportRequest) (providerImportRequest, error) {
	request.Name = strings.TrimSpace(request.Name)
	request.BaseURL = strings.TrimRight(strings.TrimSpace(request.BaseURL), "/")
	request.APIKey = strings.TrimSpace(request.APIKey)
	request.WireAPI = strings.ToLower(strings.TrimSpace(request.WireAPI))
	request.RelayMode = strings.TrimSpace(request.RelayMode)
	if request.Name == "" {
		return providerImportRequest{}, errors.New("供应商名称为空")
	}
	if request.BaseURL == "" {
		return providerImportRequest{}, errors.New("Base URL 为空")
	}
	if request.APIKey == "" {
		return providerImportRequest{}, errors.New("API Key 为空")
	}
	if request.WireAPI == "" {
		request.WireAPI = "responses"
	}
	if request.RelayMode == "" {
		request.RelayMode = "pureApi"
	}
	// Import links and pending files are untrusted. Build managed snapshots only
	// from the validated fields above instead of accepting executable TOML/JSON.
	request.ConfigContents = buildCCSConfigToml(request.BaseURL, request.APIKey, providerImportProtocol(request.WireAPI))
	request.AuthContents = buildCCSAuthJSON(request.APIKey)
	return request, nil
}

func savePendingProviderImport(request providerImportRequest) error {
	request, err := normalizeProviderImportRequest(request)
	if err != nil {
		return err
	}
	pending := request
	pending.ConfigContents = ""
	pending.AuthContents = ""
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(pendingProviderImportPath(), data)
}

func loadPendingProviderImport() (*providerImportRequest, error) {
	data, err := os.ReadFile(pendingProviderImportPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var request providerImportRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return nil, err
	}
	request, err = normalizeProviderImportRequest(request)
	if err != nil {
		return nil, err
	}
	return &request, nil
}

func clearPendingProviderImport() error {
	err := os.Remove(pendingProviderImportPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func confirmPendingProviderImport() (*providerImportResult, error) {
	request, err := loadPendingProviderImport()
	if err != nil || request == nil {
		return nil, err
	}
	result, err := importProviderRequest(*request)
	if err != nil {
		return nil, err
	}
	if err := clearPendingProviderImport(); err != nil {
		return nil, err
	}
	return &result, nil
}

func importProviderRequest(request providerImportRequest) (providerImportResult, error) {
	request, err := normalizeProviderImportRequest(request)
	if err != nil {
		return providerImportResult{}, err
	}
	settings := loadSettings()
	identity := ccsImportKey(request.Name, request.BaseURL)
	for _, profile := range settings.RelayProfiles {
		if ccsImportKey(profile.Name, firstNonEmpty(profile.UpstreamBaseURL, profile.BaseURL)) == identity {
			return providerImportResult{Imported: false, ProfileID: profile.ID, ProfileName: profile.Name}, nil
		}
	}
	existingIDs := map[string]bool{}
	for _, profile := range settings.RelayProfiles {
		existingIDs[profile.ID] = true
	}
	profile := relayProfile{
		ID:                     uniqueProfileID("import-"+sanitizeID(request.Name), existingIDs),
		Name:                   request.Name,
		BaseURL:                request.BaseURL,
		UpstreamBaseURL:        request.BaseURL,
		APIKey:                 request.APIKey,
		Protocol:               providerImportProtocol(request.WireAPI),
		RelayMode:              providerImportRelayMode(request.RelayMode),
		ConfigContents:         request.ConfigContents,
		AuthContents:           request.AuthContents,
		UseCommonConfig:        true,
		ModelInsertMode:        "patch",
		ImageGenerationEnabled: true,
	}
	settings.RelayProfiles = append(settings.RelayProfiles, profile)
	settings.ActiveRelayID = profile.ID
	if err := saveSettings(settings); err != nil {
		return providerImportResult{}, err
	}
	return providerImportResult{Imported: true, ProfileID: profile.ID, ProfileName: profile.Name}, nil
}

func providerImportProtocol(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "chat", "chat_completions", "chat-completions", "openai_chat", "openai-chat":
		return "chatCompletions"
	default:
		return "responses"
	}
}

func providerImportRelayMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "official":
		return "official"
	case "mixedapi", "mixed-api", "mixed_api":
		return "mixedApi"
	case "aggregate":
		return "aggregate"
	default:
		return "pureApi"
	}
}
