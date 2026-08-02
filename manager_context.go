package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

func (s *server) readLiveContextEntries() commandResult {
	configPath := filepath.Join(codexHomeDir(), "config.toml")
	config := readFile(configPath)
	return ok("live 工具与插件已读取。", map[string]any{
		"entries": listContextEntriesFromConfig(config),
	})
}

func (s *server) syncLiveContextEntries(args map[string]any) commandResult {
	var settings backendSettings
	if err := remarshal(mapArg(args, "request")["settings"], &settings); err != nil {
		return failed("同步 live 工具与插件失败："+err.Error(), map[string]any{"entries": emptyCodexContextEntries()})
	}
	settings = normalizeSettings(settings)
	configPath := filepath.Join(codexHomeDir(), "config.toml")
	currentConfig := readFile(configPath)
	common, _ := splitContextConfigSections(currentConfig)
	selected := filterCommonConfigForSelection(settings.RelayContextConfigContents, contextSelectionForAllEntries(settings.RelayContextConfigContents))
	updated := joinConfigSections(common, selected)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return failed("创建 Codex 配置目录失败："+err.Error(), map[string]any{"entries": emptyCodexContextEntries()})
	}
	if _, err := writeCodexConfigWithBackup(configPath, updated, "context-sync"); err != nil {
		return failed("写入 live config.toml 失败："+err.Error(), map[string]any{"entries": emptyCodexContextEntries()})
	}
	return ok("live 工具与插件已同步。", map[string]any{
		"entries": listContextEntriesFromConfig(updated),
	})
}

func (s *server) upsertContextEntry(args map[string]any) commandResult {
	request := mapArg(args, "request")
	var settings backendSettings
	if err := remarshal(request["settings"], &settings); err != nil {
		return failed("保存工具与插件失败："+err.Error(), map[string]any{"settings": loadSettings(), "entries": emptyCodexContextEntries()})
	}
	settings = normalizeSettings(settings)
	updated, err := upsertContextEntryInConfig(
		settings.RelayContextConfigContents,
		stringArg(request, "kind"),
		stringArg(request, "id"),
		stringFromAny(request["tomlBody"]),
	)
	if err != nil {
		return failed("保存工具与插件失败："+err.Error(), map[string]any{"settings": settings, "entries": emptyCodexContextEntries()})
	}
	settings.RelayContextConfigContents = updated
	settings = normalizeSettings(settings)
	return ok("工具与插件已保存。", map[string]any{
		"settings": settings,
		"entries":  listContextEntriesFromConfig(settings.RelayContextConfigContents),
	})
}

func (s *server) deleteContextEntry(args map[string]any) commandResult {
	request := mapArg(args, "request")
	var settings backendSettings
	if err := remarshal(request["settings"], &settings); err != nil {
		return failed("删除工具与插件失败："+err.Error(), map[string]any{"settings": loadSettings(), "entries": emptyCodexContextEntries()})
	}
	settings = normalizeSettings(settings)
	kind := stringArg(request, "kind")
	id := stringArg(request, "id")
	settings.RelayContextConfigContents = deleteContextEntryFromConfig(settings.RelayContextConfigContents, kind, id)
	for index := range settings.RelayProfiles {
		settings.RelayProfiles[index].ContextSelection = removeContextSelectionID(settings.RelayProfiles[index].ContextSelection, kind, id)
	}
	settings = normalizeSettings(settings)
	return ok("工具与插件已删除。", map[string]any{
		"settings": settings,
		"entries":  listContextEntriesFromConfig(settings.RelayContextConfigContents),
	})
}

func (s *server) extractRelayCommonConfig(args map[string]any) commandResult {
	configContents := stringFromAny(mapArg(args, "request")["configContents"])
	common := extractCommonRelayConfig(configContents)
	commonPart, contextPart := splitContextConfigSections(common)
	profileConfig := stripCommonConfigFromConfig(configContents, joinConfigSections(commonPart, contextPart))
	return ok("通用配置已提取。", map[string]any{
		"commonConfigContents":  commonPart,
		"contextConfigContents": contextPart,
		"profileConfigContents": profileConfig,
	})
}

func (s *server) fetchRelayProfileModels(ctx context.Context, args map[string]any) commandResult {
	var profile relayProfile
	if err := remarshal(args["profile"], &profile); err != nil {
		return failed("供应商参数错误："+err.Error(), map[string]any{"models": []string{}, "endpoint": ""})
	}
	baseURL := strings.TrimSpace(firstNonEmpty(profile.UpstreamBaseURL, profile.BaseURL))
	if baseURL == "" {
		return failed("从「"+displayRelayName(profile)+"」获取模型失败：Base URL 不能为空", map[string]any{"models": []string{}, "endpoint": ""})
	}
	source := codexModelSource{
		ID:           "relay-profile:" + strings.TrimSpace(profile.ID),
		Type:         "relay_profile",
		Name:         displayRelayName(profile),
		BaseURL:      baseURL,
		APIKey:       strings.TrimSpace(profile.APIKey),
		ProxyEnabled: profile.ProxyEnabled,
		ProxyURL:     profile.ProxyURL,
	}
	endpoint := modelsEndpoint(source.BaseURL)
	models, status := fetchModelsFromSource(ctx, source)
	if len(models) == 0 {
		message := strings.TrimSpace(stringFromAny(status["message"]))
		if message == "" {
			message = "上游没有返回可用模型"
		}
		return failed("从「"+displayRelayName(profile)+"」获取模型失败："+message, map[string]any{"models": []string{}, "endpoint": endpoint})
	}
	return ok("已从「"+displayRelayName(profile)+"」获取 "+stringFromAny(len(models))+" 个模型。", map[string]any{
		"models":   models,
		"endpoint": endpoint,
	})
}

func removeContextSelectionID(selection relayContextSelection, kind, id string) relayContextSelection {
	id = strings.TrimSpace(id)
	switch strings.TrimSpace(kind) {
	case "mcp", "mcp_server", "mcpServers":
		selection.MCPServers = removeString(selection.MCPServers, id)
	case "skill", "skills":
		selection.Skills = removeString(selection.Skills, id)
	case "plugin", "plugins":
		selection.Plugins = removeString(selection.Plugins, id)
	}
	return normalizeContextSelection(selection)
}

func removeString(values []string, target string) []string {
	var out []string
	for _, value := range values {
		if strings.TrimSpace(value) != target {
			out = append(out, value)
		}
	}
	return out
}

func extractCommonRelayConfig(configContents string) string {
	return sanitizeCommonRelayConfig(configContents)
}

func sanitizeCommonRelayConfig(configContents string) string {
	common := removeTomlTablesMatching(configContents, func(table string) bool {
		return strings.HasPrefix(table, "model_providers.")
	})
	var kept []string
	inRoot := true
	for _, line := range splitLines(common) {
		trimmed := strings.TrimSpace(line)
		if isTomlHeader(trimmed) {
			inRoot = false
		}
		if inRoot && isProviderSpecificCommonRootKey(rootLineKey(line)) {
			continue
		}
		kept = append(kept, line)
	}
	return normalizeConfigText(strings.Join(kept, "\n"))
}

func isProviderSpecificCommonRootKey(key string) bool {
	key = strings.Trim(strings.TrimSpace(key), `"'`)
	switch key {
	case "model", "model_provider", "base_url", "openai_base_url", "chatgpt_base_url", "model_catalog_json", "OPENAI_API_KEY", "codex_plus_chat_base_url":
		return true
	}
	lower := strings.ToLower(key)
	return lower == "api_key" || lower == "access_token" || lower == "bearer_token" || lower == "experimental_bearer_token" ||
		strings.HasSuffix(lower, "_api_key") || strings.HasSuffix(lower, "_access_token") || strings.HasSuffix(lower, "_bearer_token")
}

func stripCommonConfigFromConfig(configContents, commonConfig string) string {
	anchors := commonConfigAnchors(commonConfig)
	if len(anchors.rootKeys) == 0 && len(anchors.tableHeaders) == 0 {
		return normalizeConfigText(configContents)
	}
	var kept []string
	skippingTable := false
	inRoot := true
	for _, line := range splitLines(configContents) {
		trimmed := strings.TrimSpace(line)
		if isTomlHeader(trimmed) {
			inRoot = false
			skippingTable = anchors.tableHeaders[trimmed]
			if skippingTable {
				continue
			}
		}
		if skippingTable {
			continue
		}
		if inRoot {
			key := rootLineKey(line)
			if key != "" && anchors.rootKeys[key] {
				continue
			}
		}
		kept = append(kept, line)
	}
	return normalizeConfigText(strings.Join(kept, "\n"))
}

type configAnchorSet struct {
	rootKeys     map[string]bool
	tableHeaders map[string]bool
}

func commonConfigAnchors(config string) configAnchorSet {
	anchors := configAnchorSet{rootKeys: map[string]bool{}, tableHeaders: map[string]bool{}}
	inRoot := true
	for _, line := range splitLines(config) {
		trimmed := strings.TrimSpace(line)
		if isTomlHeader(trimmed) {
			inRoot = false
			anchors.tableHeaders[trimmed] = true
			continue
		}
		if inRoot {
			if key := rootLineKey(line); key != "" {
				anchors.rootKeys[key] = true
			}
		}
	}
	return anchors
}
