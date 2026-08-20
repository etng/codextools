package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type providerSyncTargetOption struct {
	ID                string   `json:"id"`
	Sources           []string `json:"sources"`
	IsCurrentProvider bool     `json:"isCurrentProvider"`
	IsManual          bool     `json:"isManual"`
	IsSaved           bool     `json:"isSaved"`
}

func (s *server) loadProviderSyncTargets() commandResult {
	settings := loadSettings()
	targets := discoverProviderSyncTargets(codexHomeDir(), settings)
	return ok("Provider 同步目标已加载。", map[string]any{
		"currentProvider": targets.currentProvider,
		"targets":         targets.targets,
	})
}

type providerSyncTargetList struct {
	currentProvider string
	targets         []providerSyncTargetOption
}

func discoverProviderSyncTargets(home string, settings backendSettings) providerSyncTargetList {
	currentProvider := readCurrentProvider(filepath.Join(home, "config.toml"))
	sources := map[string]map[string]bool{}
	add := func(provider, source string) {
		provider = strings.TrimSpace(provider)
		if !validProviderSyncTargetID(provider) {
			return
		}
		if sources[provider] == nil {
			sources[provider] = map[string]bool{}
		}
		sources[provider][source] = true
	}
	for _, provider := range configuredProviderSyncIDs(filepath.Join(home, "config.toml")) {
		add(provider, "config")
	}
	add(currentProvider, "config")
	for _, provider := range rolloutProviderSyncIDs(home) {
		add(provider, "rollout")
	}
	for _, path := range codexSessionDBPaths(home) {
		for _, provider := range sqliteProviderSyncIDs(path) {
			add(provider, "sqlite")
		}
	}
	manualIDs := append(append([]string{}, settings.ProviderSyncManualProviders...), settings.ProviderSyncSavedProviders...)
	for _, provider := range manualIDs {
		add(provider, "manual")
	}
	targets := make([]providerSyncTargetOption, 0, len(sources))
	for provider, sourceSet := range sources {
		list := make([]string, 0, len(sourceSet))
		for source := range sourceSet {
			list = append(list, source)
		}
		sort.Strings(list)
		targets = append(targets, providerSyncTargetOption{
			ID: provider, Sources: list, IsCurrentProvider: provider == currentProvider,
			IsManual: providerSyncStringContains(settings.ProviderSyncManualProviders, provider),
			IsSaved:  providerSyncStringContains(settings.ProviderSyncSavedProviders, provider),
		})
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].IsCurrentProvider != targets[j].IsCurrentProvider {
			return targets[i].IsCurrentProvider
		}
		return targets[i].ID < targets[j].ID
	})
	return providerSyncTargetList{currentProvider: currentProvider, targets: targets}
}

func validProviderSyncTargetID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func configuredProviderSyncIDs(path string) []string {
	ids := map[string]bool{"openai": true}
	for _, line := range strings.Split(readFile(path), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[model_providers.") || !strings.HasSuffix(line, "]") {
			continue
		}
		provider := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[model_providers."), "]"))
		if validProviderSyncTargetID(provider) {
			ids[provider] = true
		}
	}
	return sortedProviderSyncIDs(ids)
}

func rolloutProviderSyncIDs(home string) []string {
	ids := map[string]bool{}
	for _, dirname := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(home, dirname)
		if !isDir(root) {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			data, _, readErr := readProviderSyncMetadata(path)
			if readErr != nil {
				return nil
			}
			for _, line := range strings.Split(string(data), "\n") {
				var record map[string]any
				if json.Unmarshal([]byte(line), &record) != nil || stringFromAny(record["type"]) != "session_meta" {
					continue
				}
				provider := stringFromAny(mapFromAny(record["payload"])["model_provider"])
				if validProviderSyncTargetID(provider) {
					ids[provider] = true
				}
			}
			return nil
		})
	}
	return sortedProviderSyncIDs(ids)
}

func sqliteProviderSyncIDs(path string) []string {
	ids := map[string]bool{}
	db, err := openSQLite(path)
	if err != nil {
		return nil
	}
	defer db.Close()
	for _, table := range []string{"threads", "local_thread_catalog"} {
		columns, columnsErr := sqliteTableColumns(db, table)
		if columnsErr != nil || !providerSyncStringContains(columns, "model_provider") {
			continue
		}
		rows, queryErr := db.Query("SELECT DISTINCT COALESCE(model_provider, '') FROM " + quoteSQLiteIdentifier(table) + " WHERE COALESCE(model_provider, '') <> ''")
		if queryErr != nil {
			continue
		}
		for rows.Next() {
			var provider string
			if rows.Scan(&provider) == nil && validProviderSyncTargetID(provider) {
				ids[provider] = true
			}
		}
		_ = rows.Close()
	}
	return sortedProviderSyncIDs(ids)
}

func sortedProviderSyncIDs(ids map[string]bool) []string {
	values := make([]string, 0, len(ids))
	for id := range ids {
		values = append(values, id)
	}
	sort.Strings(values)
	return values
}

func providerSyncStringContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
