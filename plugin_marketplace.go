package main

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed assets/plugin-marketplaces/openai-curated-remote.zip
var openAICuratedRemoteMarketplaceZip []byte

const (
	openAICuratedMarketplaceName       = "openai-curated"
	openAIAPICuratedMarketplaceName    = "openai-api-curated"
	openAICuratedRemoteMarketplaceName = "openai-curated-remote"
	roleSpecificMarketplaceName        = "role-specific-plugins"
	openAIPluginsZipURL                = "https://codeload.github.com/openai/plugins/zip/refs/heads/main"
	openAIPluginsDownloadLimitBytes    = 128 * 1024 * 1024
)

type pluginMarketplaceStatus struct {
	CodexHome        string  `json:"codexHome"`
	MarketplaceRoot  *string `json:"marketplaceRoot,omitempty"`
	ConfigRegistered bool    `json:"configRegistered"`
	NeedsRepair      bool    `json:"needsRepair"`
}

type pluginMarketplaceRepairPayload struct {
	CodexHome       string  `json:"codexHome"`
	MarketplaceRoot *string `json:"marketplaceRoot,omitempty"`
	Initialized     bool    `json:"initialized"`
	Configured      bool    `json:"configured"`
	NeedsRepair     bool    `json:"needsRepair"`
}

type remotePluginMarketplacePayload struct {
	CodexHome        string  `json:"codexHome"`
	MarketplaceRoot  *string `json:"marketplaceRoot,omitempty"`
	ConfigRegistered bool    `json:"configRegistered"`
	NeedsRepair      bool    `json:"needsRepair"`
	PluginCount      int     `json:"pluginCount"`
	SkillCount       int     `json:"skillCount"`
}

func (s *server) pluginMarketplaceStatus() commandResult {
	status := openAICuratedMarketplaceStatus(codexHomeDir())
	message := "插件市场已可用。"
	if status.NeedsRepair {
		message = "插件市场需要初始化或注册。"
	}
	payload := map[string]any{
		"codexHome":        status.CodexHome,
		"marketplaceRoot":  nullableStringPtr(status.MarketplaceRoot),
		"configRegistered": status.ConfigRegistered,
		"needsRepair":      status.NeedsRepair,
	}
	return ok(message, payload)
}

func (s *server) repairPluginMarketplace(ctx context.Context) commandResult {
	home := codexHomeDir()
	initialized := false
	if localOpenAICuratedMarketplaceRoot(home) == "" {
		if err := initializeOpenAICuratedMarketplaceFromGitHub(ctx, home); err != nil {
			status := openAICuratedMarketplaceStatus(home)
			return failed("插件市场修复失败："+err.Error(), map[string]any{
				"codexHome":       status.CodexHome,
				"marketplaceRoot": nullableStringPtr(status.MarketplaceRoot),
				"initialized":     initialized,
				"configured":      status.ConfigRegistered,
				"needsRepair":     status.NeedsRepair,
			})
		}
		initialized = true
	}
	configured, err := ensureOpenAICuratedMarketplaceConfig(home)
	if err != nil {
		status := openAICuratedMarketplaceStatus(home)
		return failed("插件市场修复失败："+err.Error(), map[string]any{
			"codexHome":       status.CodexHome,
			"marketplaceRoot": nullableStringPtr(status.MarketplaceRoot),
			"initialized":     initialized,
			"configured":      status.ConfigRegistered,
			"needsRepair":     status.NeedsRepair,
		})
	}
	status := openAICuratedMarketplaceStatus(home)
	message := "插件市场已可用，无需修复。"
	if initialized {
		message = "插件市场已从 openai/plugins 初始化并注册。"
	} else if configured {
		message = "已注册本地插件市场。"
	}
	return ok(message, map[string]any{
		"codexHome":       status.CodexHome,
		"marketplaceRoot": nullableStringPtr(status.MarketplaceRoot),
		"initialized":     initialized,
		"configured":      configured,
		"needsRepair":     false,
	})
}

func (s *server) remotePluginMarketplaceStatus() commandResult {
	payload := currentRemotePluginMarketplacePayload(codexHomeDir())
	message := "官方远端插件缓存已可用。"
	if payload.NeedsRepair {
		message = "官方远端插件缓存需要释放或注册。"
	}
	return ok(message, remotePluginMarketplaceValue(payload))
}

func (s *server) repairRemotePluginMarketplace() commandResult {
	home := codexHomeDir()
	initialized := false
	if localNamedPluginMarketplaceRoot(filepath.Join(home, ".tmp", "plugins-remote"), openAICuratedRemoteMarketplaceName) == "" {
		if err := installOpenAICuratedRemoteMarketplaceZip(home, openAICuratedRemoteMarketplaceZip); err != nil {
			return failed("官方远端插件缓存修复失败："+err.Error(), remotePluginMarketplaceValue(currentRemotePluginMarketplacePayload(home)))
		}
		initialized = true
	}
	configured, err := ensureOpenAICuratedMarketplaceConfig(home)
	if err != nil {
		return failed("官方远端插件缓存修复失败："+err.Error(), remotePluginMarketplaceValue(currentRemotePluginMarketplacePayload(home)))
	}
	payload := currentRemotePluginMarketplacePayload(home)
	message := "官方远端插件缓存已可用，无需修复。"
	if initialized {
		message = "已释放并注册内置官方远端插件缓存。"
	} else if configured {
		message = "已注册官方远端插件缓存。"
	}
	return ok(message, remotePluginMarketplaceValue(payload))
}

func remotePluginMarketplaceValue(payload remotePluginMarketplacePayload) map[string]any {
	return map[string]any{
		"codexHome":        payload.CodexHome,
		"marketplaceRoot":  nullableStringPtr(payload.MarketplaceRoot),
		"configRegistered": payload.ConfigRegistered,
		"needsRepair":      payload.NeedsRepair,
		"pluginCount":      payload.PluginCount,
		"skillCount":       payload.SkillCount,
	}
}

func currentRemotePluginMarketplacePayload(home string) remotePluginMarketplacePayload {
	root := localNamedPluginMarketplaceRoot(filepath.Join(home, ".tmp", "plugins-remote"), openAICuratedRemoteMarketplaceName)
	var rootPtr *string
	pluginCount, skillCount := 0, 0
	if root != "" {
		rootPtr = &root
		pluginCount = len(localMarketplacePluginNames(root, openAICuratedRemoteMarketplaceName))
		for _, name := range localMarketplacePluginNames(root, openAICuratedRemoteMarketplaceName) {
			skillRoot := filepath.Join(root, "plugins", name, "skills")
			entries, _ := os.ReadDir(skillRoot)
			for _, entry := range entries {
				if entry.IsDir() && fileExists(filepath.Join(skillRoot, entry.Name(), "SKILL.md")) {
					skillCount++
				}
			}
		}
	}
	registered := root != "" && marketplaceConfigPointsToRoot(home, openAICuratedRemoteMarketplaceName, root)
	return remotePluginMarketplacePayload{CodexHome: home, MarketplaceRoot: rootPtr, ConfigRegistered: registered, NeedsRepair: root == "" || !registered, PluginCount: pluginCount, SkillCount: skillCount}
}

func openAICuratedMarketplaceStatus(home string) pluginMarketplaceStatus {
	root := localOpenAICuratedMarketplaceRoot(home)
	var rootPtr *string
	if root != "" {
		rootPtr = &root
	}
	registered := root != "" && marketplaceConfigPointsToRoot(home, openAICuratedMarketplaceName, root)
	return pluginMarketplaceStatus{
		CodexHome:        home,
		MarketplaceRoot:  rootPtr,
		ConfigRegistered: registered,
		NeedsRepair:      root == "" || !registered,
	}
}

func localOpenAICuratedMarketplaceRoot(home string) string {
	root := filepath.Join(home, ".tmp", "plugins")
	manifestPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return ""
	}
	var manifest map[string]any
	if json.Unmarshal(data, &manifest) != nil {
		return ""
	}
	if stringFromAny(manifest["name"]) != openAICuratedMarketplaceName {
		return ""
	}
	plugins, _ := manifest["plugins"].([]any)
	if len(plugins) == 0 || !isDir(filepath.Join(root, "plugins")) {
		return ""
	}
	return root
}

func marketplaceConfigPointsToRoot(home, name, root string) bool {
	contents := readFile(filepath.Join(home, "config.toml"))
	values := tableValues(contents, "marketplaces."+name)
	return strings.TrimSpace(unquoteToml(values["source_type"])) == "local" && samePath(strings.TrimSpace(unquoteToml(values["source"])), root)
}

func ensureOpenAICuratedMarketplaceConfig(home string) (bool, error) {
	path := filepath.Join(home, "config.toml")
	contents := readFile(path)
	updated := contents
	if root := localOpenAICuratedMarketplaceRoot(home); root != "" {
		for _, name := range []string{openAICuratedMarketplaceName, openAIAPICuratedMarketplaceName} {
			updated = repairCodexMarketplaceTable(updated, marketplaceSpec{Name: name, Source: root})
		}
	}
	if root := localNamedPluginMarketplaceRoot(filepath.Join(home, ".tmp", "plugins-remote"), openAICuratedRemoteMarketplaceName); root != "" {
		updated = repairCodexMarketplaceTable(updated, marketplaceSpec{Name: openAICuratedRemoteMarketplaceName, Source: root})
	}
	if root := localNamedPluginMarketplaceRoot(filepath.Join(home, ".tmp", "marketplaces", roleSpecificMarketplaceName), roleSpecificMarketplaceName); root != "" {
		updated = repairCodexMarketplaceTable(updated, marketplaceSpec{Name: roleSpecificMarketplaceName, Source: root})
		for _, pluginName := range localMarketplacePluginNames(root, roleSpecificMarketplaceName) {
			table := "plugins." + quoteToml(pluginName+"@"+roleSpecificMarketplaceName)
			if !hasTable(updated, table) {
				updated = appendTomlBlock(updated, []string{"[" + table + "]", "enabled = true"})
			}
		}
	}
	if updated == contents {
		return false, nil
	}
	if _, err := writeCodexConfigWithBackup(path, updated, "plugin-marketplace"); err != nil {
		return false, err
	}
	return true, nil
}

func localNamedPluginMarketplaceRoot(root, expectedName string) string {
	manifestPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return ""
	}
	var manifest map[string]any
	if json.Unmarshal(data, &manifest) != nil || stringFromAny(manifest["name"]) != expectedName {
		return ""
	}
	plugins, _ := manifest["plugins"].([]any)
	if len(plugins) == 0 || !isDir(filepath.Join(root, "plugins")) {
		return ""
	}
	return root
}

func localMarketplacePluginNames(root, expectedName string) []string {
	data, err := os.ReadFile(filepath.Join(root, ".agents", "plugins", "marketplace.json"))
	if err != nil {
		return nil
	}
	var manifest map[string]any
	if json.Unmarshal(data, &manifest) != nil || stringFromAny(manifest["name"]) != expectedName {
		return nil
	}
	var names []string
	for _, raw := range anySlice(manifest["plugins"]) {
		plugin, _ := raw.(map[string]any)
		if name := strings.TrimSpace(stringFromAny(plugin["name"])); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func localPluginMarketplaces(home string) []any {
	marketplaceDir := filepath.Join(home, ".tmp", "plugins", ".agents", "plugins")
	candidates := []string{
		filepath.Join(marketplaceDir, "marketplace.json"),
		filepath.Join(marketplaceDir, "api_marketplace.json"),
		filepath.Join(home, ".tmp", "plugins-remote", ".agents", "plugins", "marketplace.json"),
	}
	installed := installedPluginIDs(readFile(filepath.Join(home, "config.toml")))
	marketplaces := make([]any, 0, len(candidates))
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var marketplace map[string]any
		if json.Unmarshal(data, &marketplace) != nil || strings.TrimSpace(stringFromAny(marketplace["name"])) == "" {
			continue
		}
		expandLocalPluginMarketplace(marketplace, path, installed)
		if marketplace["path"] == nil {
			marketplace["path"] = path
		}
		marketplaces = append(marketplaces, marketplace)
	}
	return marketplaces
}

func installedPluginIDs(config string) map[string]bool {
	installed := map[string]bool{}
	for _, line := range splitLines(config) {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[plugins.") || !strings.HasSuffix(trimmed, "]") {
			continue
		}
		table := strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
		values := tableValues(config, table)
		if strings.EqualFold(strings.TrimSpace(values["enabled"]), "true") {
			id := strings.TrimPrefix(table, "plugins.")
			installed[strings.TrimSpace(unquoteToml(id))] = true
		}
	}
	return installed
}

func expandLocalPluginMarketplace(marketplace map[string]any, marketplacePath string, installed map[string]bool) {
	marketplaceName := stringFromAny(marketplace["name"])
	plugins, _ := marketplace["plugins"].([]any)
	marketplaceRoot := filepath.Dir(filepath.Dir(filepath.Dir(marketplacePath)))
	for _, raw := range plugins {
		plugin, _ := raw.(map[string]any)
		if plugin == nil {
			continue
		}
		name := strings.TrimSpace(firstNonEmpty(stringFromAny(plugin["name"]), strings.SplitN(stringFromAny(plugin["id"]), "@", 2)[0]))
		if name == "" {
			continue
		}
		pluginRoot := filepath.Join(marketplaceRoot, "plugins", name)
		if data, err := os.ReadFile(filepath.Join(pluginRoot, ".codex-plugin", "plugin.json")); err == nil {
			var manifest map[string]any
			if json.Unmarshal(data, &manifest) == nil {
				for key, value := range manifest {
					if plugin[key] == nil {
						plugin[key] = value
					}
				}
			}
		}
		absolutizePluginAssetPaths(plugin, pluginRoot)
		if plugin["name"] == nil {
			plugin["name"] = name
		}
		if plugin["id"] == nil {
			plugin["id"] = name + "@" + marketplaceName
		}
		if plugin["marketplaceName"] == nil {
			plugin["marketplaceName"] = marketplaceName
		}
		if plugin["marketplacePath"] == nil {
			plugin["marketplacePath"] = marketplaceName
		}
		if plugin["keywords"] == nil {
			plugin["keywords"] = []any{}
		}
		plugin["installed"] = installed[name+"@"+marketplaceName]
	}
}

func absolutizePluginAssetPaths(plugin map[string]any, root string) {
	for _, key := range []string{"composerIconPath", "logoPath"} {
		absolutizePluginAssetPath(plugin, key, root)
	}
	if pluginInterface, ok := plugin["interface"].(map[string]any); ok {
		for _, key := range []string{"composerIcon", "composerIconPath", "logo", "logoPath"} {
			absolutizePluginAssetPath(pluginInterface, key, root)
		}
	}
}

func absolutizePluginAssetPath(object map[string]any, key, root string) {
	value := strings.TrimSpace(stringFromAny(object[key]))
	if value == "" || filepath.IsAbs(value) || strings.HasPrefix(value, "data:") || strings.HasPrefix(value, "http:") || strings.HasPrefix(value, "https:") || strings.HasPrefix(value, "file:") {
		return
	}
	object[key] = filepath.Join(root, strings.TrimPrefix(filepath.FromSlash(value), "."+string(filepath.Separator)))
}

func initializeOpenAICuratedMarketplaceFromGitHub(ctx context.Context, home string) error {
	bytes, err := downloadOpenAIPluginsZip(ctx)
	if err != nil {
		return err
	}
	return installOpenAIPluginsZip(home, bytes)
}

func downloadOpenAIPluginsZip(ctx context.Context) ([]byte, error) {
	client := &http.Client{Timeout: 90 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openAIPluginsZipURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/zip")
	req.Header.Set("user-agent", appName+"/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai/plugins marketplace download returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, openAIPluginsDownloadLimitBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > openAIPluginsDownloadLimitBytes {
		return nil, fmt.Errorf("openai/plugins marketplace download is too large: %d bytes", len(body))
	}
	return body, nil
}

func installOpenAIPluginsZip(home string, data []byte) error {
	destination := filepath.Join(home, ".tmp", "plugins")
	stagingParent := filepath.Join(home, ".tmp")
	if err := os.MkdirAll(stagingParent, 0o755); err != nil {
		return err
	}
	staging := filepath.Join(stagingParent, fmt.Sprintf("plugins-download-%d", time.Now().UnixMilli()))
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractOpenAIPluginsZip(data, staging); err != nil {
		return err
	}
	if localOpenAICuratedMarketplaceRoot(staging) == "" {
		return errors.New("downloaded openai/plugins marketplace is invalid")
	}
	if err := replaceDirectory(destination, staging); err != nil {
		return err
	}
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func installOpenAICuratedRemoteMarketplaceZip(home string, data []byte) error {
	if len(data) == 0 {
		return errors.New("embedded official remote plugin marketplace is empty")
	}
	destination := filepath.Join(home, ".tmp", "plugins-remote")
	stagingParent := filepath.Join(home, ".tmp")
	if err := os.MkdirAll(stagingParent, 0o755); err != nil {
		return err
	}
	staging := filepath.Join(stagingParent, fmt.Sprintf("plugins-remote-embedded-%d", time.Now().UnixMilli()))
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := extractPluginMarketplaceZipExact(data, staging); err != nil {
		return err
	}
	if localNamedPluginMarketplaceRoot(staging, openAICuratedRemoteMarketplaceName) == "" {
		return errors.New("embedded official remote plugin marketplace is invalid")
	}
	return replaceDirectory(destination, staging)
}

func extractPluginMarketplaceZipExact(data []byte, destination string) error {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, file := range reader.File {
		relative, err := safePluginMarketplaceZipPath(file.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		input, err := file.Open()
		if err != nil {
			return err
		}
		writeErr := writeZipFile(target, input, file.Mode().Perm())
		closeErr := input.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func safePluginMarketplaceZipPath(name string) (string, error) {
	name = filepath.ToSlash(strings.TrimSpace(name))
	if name == "" || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("zip entry has unsafe path: %q", name)
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("zip entry escapes destination: %q", name)
		}
	}
	return filepath.Join(parts...), nil
}

func extractOpenAIPluginsZip(data []byte, destination string) error {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, file := range reader.File {
		relative, ok := zipEntryRelativePath(file.Name)
		if !ok {
			continue
		}
		target := filepath.Join(destination, relative)
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, file.Mode().Perm()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		input, err := file.Open()
		if err != nil {
			return err
		}
		err = writeZipFile(target, input, file.Mode().Perm())
		closeErr := input.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func zipEntryRelativePath(name string) (string, bool) {
	name = filepath.ToSlash(strings.TrimSpace(name))
	parts := strings.Split(name, "/")
	if len(parts) < 2 {
		return "", false
	}
	relativeParts := parts[1:]
	for _, part := range relativeParts {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return filepath.Join(relativeParts...), true
}

func writeZipFile(path string, input io.Reader, perm os.FileMode) error {
	output, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
