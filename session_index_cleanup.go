package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type sessionIndexCleanupCandidate struct {
	ID         string `json:"id"`
	ThreadName string `json:"threadName"`
	UpdatedAt  string `json:"updatedAt"`
}

type sessionIndexCleanupPreview struct {
	SnapshotSHA256 string                         `json:"snapshotSha256"`
	Candidates     []sessionIndexCleanupCandidate `json:"candidates"`
}

type sessionIndexCleanupResult struct {
	PrunedEntries int    `json:"prunedEntries"`
	BackupDir     string `json:"backupDir,omitempty"`
}

type sessionIndexCleanupPlan struct {
	Path           string
	Original       []byte
	SnapshotSHA256 string
	Candidates     []sessionIndexCleanupCandidate
}

type sessionIndexCleanupApplyError struct {
	Message   string
	BackupDir string
}

func (e *sessionIndexCleanupApplyError) Error() string { return e.Message }

var sessionIndexCleanupBeforeWrite = func() {}

var sessionIndexReferenceColumns = [][2]string{
	{"threads", "id"},
	{"local_thread_catalog", "thread_id"},
	{"automation_runs", "thread_id"},
	{"inbox_items", "thread_id"},
	{"sessions", "id"},
	{"messages", "session_id"},
	{"thread_dynamic_tools", "thread_id"},
	{"thread_goals", "thread_id"},
	{"thread_spawn_edges", "parent_thread_id"},
	{"thread_spawn_edges", "child_thread_id"},
	{"stage1_outputs", "thread_id"},
	{"agent_job_items", "assigned_thread_id"},
}

func previewSessionIndexCleanup(home string) (sessionIndexCleanupPreview, error) {
	ids, err := collectLiveSessionThreadIDs(home, codexThreadReferenceDBPaths(home))
	if err != nil {
		return sessionIndexCleanupPreview{}, err
	}
	plan, err := planSessionIndexCleanup(filepath.Join(home, "session_index.jsonl"), ids)
	if err != nil {
		return sessionIndexCleanupPreview{}, err
	}
	if plan == nil {
		return sessionIndexCleanupPreview{SnapshotSHA256: sessionIndexSHA256(nil), Candidates: []sessionIndexCleanupCandidate{}}, nil
	}
	return sessionIndexCleanupPreview{SnapshotSHA256: plan.SnapshotSHA256, Candidates: plan.Candidates}, nil
}

func applySessionIndexCleanup(home, expectedSnapshotSHA256 string, confirmedThreadIDs []string, requireStoppedApp bool) (sessionIndexCleanupResult, error) {
	if requireStoppedApp {
		if err := ensureProviderSyncWritersStopped(); err != nil {
			return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{Message: err.Error()}
		}
	}
	release, err := acquireProviderSyncMutationGuards(home, "session-index-cleanup")
	if err != nil {
		return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{Message: err.Error()}
	}
	defer release()

	ids, err := collectLiveSessionThreadIDs(home, codexThreadReferenceDBPaths(home))
	if err != nil {
		return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{Message: err.Error()}
	}
	plan, err := planSessionIndexCleanup(filepath.Join(home, "session_index.jsonl"), ids)
	if err != nil {
		return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{Message: err.Error()}
	}
	if plan == nil {
		return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{Message: "session_index.jsonl 不存在，无法清理"}
	}
	if strings.TrimSpace(expectedSnapshotSHA256) == "" || plan.SnapshotSHA256 != strings.TrimSpace(expectedSnapshotSHA256) {
		return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{Message: "session_index.jsonl 已在预览后发生变化；为避免覆盖 Codex 新内容，本次清理已中止，请重新预览"}
	}

	candidateIDs := make(map[string]bool, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		candidateIDs[candidate.ID] = true
	}
	selectedIDs := uniqueNonEmptyStrings(confirmedThreadIDs)
	for _, id := range selectedIDs {
		if !candidateIDs[id] {
			return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{Message: "确认列表已过期或包含非候选任务；本次清理未执行，请重新预览"}
		}
	}
	next, removed := filterSessionIndex(plan.Original, selectedIDs)
	if removed == 0 {
		return sessionIndexCleanupResult{PrunedEntries: 0}, nil
	}
	backupDir, err := createSessionIndexCleanupBackup(home, plan, removed)
	if err != nil {
		return sessionIndexCleanupResult{}, cleanupApplyError(err, backupDir)
	}

	sessionIndexCleanupBeforeWrite()
	current, err := os.ReadFile(plan.Path)
	if err != nil {
		return sessionIndexCleanupResult{}, cleanupApplyError(err, backupDir)
	}
	if !equalBytes(current, plan.Original) {
		return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{
			Message:   "session_index.jsonl 在写入前再次发生变化；未覆盖 Codex 新内容，请重新预览",
			BackupDir: backupDir,
		}
	}
	if requireStoppedApp {
		if err := ensureProviderSyncWritersStopped(); err != nil {
			return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{Message: err.Error(), BackupDir: backupDir}
		}
	}
	if err := atomicWrite(plan.Path, next); err != nil {
		return sessionIndexCleanupResult{}, &sessionIndexCleanupApplyError{
			Message:   "原子写入 session_index.jsonl 失败；原文件未被主动覆盖，可从备份目录手动恢复：" + err.Error(),
			BackupDir: backupDir,
		}
	}
	appendDiagnosticLog("provider_sync.session_index_cleanup", map[string]any{
		"pruned_entries": removed,
		"backup_dir":     backupDir,
	})
	return sessionIndexCleanupResult{PrunedEntries: removed, BackupDir: backupDir}, nil
}

func cleanupApplyError(err error, backupDir string) error {
	if err == nil {
		return nil
	}
	return &sessionIndexCleanupApplyError{Message: err.Error(), BackupDir: backupDir}
}

func planSessionIndexCleanup(path string, liveThreadIDs map[string]bool) (*sessionIndexCleanupPlan, error) {
	original, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	candidates := make([]sessionIndexCleanupCandidate, 0)
	forEachSessionIndexLine(original, func(line []byte, _ []byte) {
		if candidate, ok := knownSessionIndexCandidate(line); ok && !liveThreadIDs[candidate.ID] {
			candidates = append(candidates, candidate)
		}
	})
	return &sessionIndexCleanupPlan{
		Path:           path,
		Original:       original,
		SnapshotSHA256: sessionIndexSHA256(original),
		Candidates:     candidates,
	}, nil
}

func knownSessionIndexCandidate(line []byte) (sessionIndexCleanupCandidate, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(line, &object); err != nil || len(object) != 3 {
		return sessionIndexCleanupCandidate{}, false
	}
	for _, key := range []string{"id", "thread_name", "updated_at"} {
		if _, ok := object[key]; !ok {
			return sessionIndexCleanupCandidate{}, false
		}
	}
	var id, threadName, updatedAt string
	if json.Unmarshal(object["id"], &id) != nil || json.Unmarshal(object["thread_name"], &threadName) != nil || json.Unmarshal(object["updated_at"], &updatedAt) != nil {
		return sessionIndexCleanupCandidate{}, false
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(updatedAt) == "" {
		return sessionIndexCleanupCandidate{}, false
	}
	return sessionIndexCleanupCandidate{ID: id, ThreadName: threadName, UpdatedAt: updatedAt}, true
}

func filterSessionIndex(original []byte, selected []string) ([]byte, int) {
	selectedIDs := map[string]bool{}
	for _, id := range selected {
		selectedIDs[strings.TrimSpace(id)] = true
	}
	next := make([]byte, 0, len(original))
	removed := 0
	forEachSessionIndexLine(original, func(line, ending []byte) {
		if candidate, ok := knownSessionIndexCandidate(line); ok && selectedIDs[candidate.ID] {
			removed++
			return
		}
		next = append(next, line...)
		next = append(next, ending...)
	})
	return next, removed
}

func forEachSessionIndexLine(data []byte, visit func(line, ending []byte)) {
	for len(data) > 0 {
		newline := -1
		for index, value := range data {
			if value == '\n' {
				newline = index
				break
			}
		}
		if newline < 0 {
			visit(data, nil)
			return
		}
		line := data[:newline]
		ending := data[newline : newline+1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
			ending = data[newline-1 : newline+1]
		}
		visit(line, ending)
		data = data[newline+1:]
	}
}

func sessionIndexSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func collectLiveSessionThreadIDs(home string, sqlitePaths []string) (map[string]bool, error) {
	ids := map[string]bool{}
	files, err := collectRolloutPaths(home)
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		if id := rolloutThreadIDFromFilename(filepath.Base(path)); id != "" {
			ids[id] = true
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		forEachSessionIndexLine(data, func(line, _ []byte) {
			var record map[string]any
			if json.Unmarshal(line, &record) != nil || stringFromAny(record["type"]) != "session_meta" {
				return
			}
			payload, _ := record["payload"].(map[string]any)
			if id := strings.TrimSpace(stringFromAny(payload["id"])); id != "" {
				ids[id] = true
			}
		})
	}
	for _, path := range sqlitePaths {
		dbIDs, err := sessionThreadIDsFromSQLite(path)
		if err != nil {
			return nil, fmt.Errorf("读取会话引用数据库 %s 失败：%w", path, err)
		}
		for id := range dbIDs {
			ids[id] = true
		}
	}
	return ids, nil
}

func collectRolloutPaths(home string) ([]string, error) {
	paths := []string{}
	for _, dirname := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(home, dirname)
		if !isDir(root) {
			continue
		}
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
				paths = append(paths, path)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func rolloutThreadIDFromFilename(name string) string {
	if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
		return ""
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(name, "rollout-"), ".jsonl")
	if len(stem) < 36 {
		return ""
	}
	candidate := stem[len(stem)-36:]
	for index, value := range candidate {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if value != '-' {
				return ""
			}
			continue
		}
		if !((value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')) {
			return ""
		}
	}
	return candidate
}

func sessionThreadIDsFromSQLite(path string) (map[string]bool, error) {
	ids := map[string]bool{}
	if !fileExists(path) {
		return ids, nil
	}
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	for _, reference := range sessionIndexReferenceColumns {
		table, column := reference[0], reference[1]
		columns, err := sqliteTableColumns(db, table)
		if err != nil {
			return nil, err
		}
		if !containsString(columns, column) {
			continue
		}
		query := "SELECT DISTINCT " + quoteSQLiteIdentifier(column) + " FROM " + quoteSQLiteIdentifier(table) + " WHERE COALESCE(" + quoteSQLiteIdentifier(column) + ", '') <> ''"
		rows, err := db.Query(query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if id = strings.TrimSpace(id); id != "" {
				ids[id] = true
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func codexThreadReferenceDBPaths(home string) []string {
	paths := append([]string{}, codexSessionDBPaths(home)...)
	sqliteDir := filepath.Join(codexSQLiteHome(home), "sqlite")
	if entries, err := os.ReadDir(sqliteDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".sqlite3") {
				continue
			}
			paths = append(paths, filepath.Join(sqliteDir, entry.Name()))
		}
	}
	return uniqueExistingPaths(paths)
}

func uniqueExistingPaths(paths []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, path := range paths {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "" || seen[path] || !fileExists(path) {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func createSessionIndexCleanupBackup(home string, plan *sessionIndexCleanupPlan, removed int) (string, error) {
	root := filepath.Join(home, "backups_state", "provider-sync")
	base := time.Now().UTC().Format("20060102-150405")
	backupDir := filepath.Join(root, base)
	for suffix := 1; fileExists(backupDir) || isDir(backupDir); suffix++ {
		backupDir = filepath.Join(root, fmt.Sprintf("%s-%d", base, suffix))
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(backupDir, "session_index.jsonl"), plan.Original, 0o600); err != nil {
		return backupDir, err
	}
	metadata := map[string]any{
		"version":                   1,
		"namespace":                 "provider-sync-session-index-cleanup",
		"codexHome":                 home,
		"createdAt":                 time.Now().UTC().Format(time.RFC3339),
		"snapshotSha256":            plan.SnapshotSHA256,
		"prunedSessionIndexEntries": removed,
		"managedBy":                 "CodexTools provider sync",
	}
	if err := atomicWriteJSON(filepath.Join(backupDir, "metadata.json"), metadata); err != nil {
		return backupDir, err
	}
	return backupDir, nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
