package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var remoteControlRecoveryMu sync.Mutex

var remoteControlRecoveryInFlight = struct {
	sync.Mutex
	keys map[string]struct{}
}{keys: map[string]struct{}{}}

type pendingRemoteControlRecovery struct {
	ThreadID       string `json:"threadId"`
	ProfileID      string `json:"profileId"`
	TargetProvider string `json:"targetProvider"`
	CreatedAt      int64  `json:"createdAt"`
}

type pendingRemoteControlRecoveryState struct {
	Version  int                            `json:"version"`
	Requests []pendingRemoteControlRecovery `json:"requests"`
}

func pendingRemoteControlRecoveryPath() string {
	return filepath.Join(stateDir(), "pending-remote-control-recovery.json")
}

func recoverRemoteControlSessionValue(payload map[string]any) map[string]any {
	threadID := bareSessionID(firstString(payload["thread_id"], payload["threadId"]))
	if threadID == "" || len(threadID) > 128 {
		return map[string]any{"status": "failed", "message": "Remote Control recovery requires a valid thread id"}
	}
	settings := loadSettings()
	active := activeRelayProfile(settings)
	if !settings.RelayProfilesEnabled || active.RelayMode != "mixedApi" {
		return map[string]any{"status": "skipped", "message": "Remote Control session recovery is disabled for the active profile"}
	}
	targetProvider := codexModelProviderForRelayProfile(codexHomeDir(), active)
	if !validProviderSyncTargetID(targetProvider) || targetProvider == "openai" {
		return map[string]any{"status": "skipped", "message": "Remote Control session recovery requires a non-openai target provider"}
	}
	requestKey := threadID + "\x00" + targetProvider
	if !beginRemoteControlRecovery(requestKey) {
		return map[string]any{"status": "in_progress", "message": "Remote Control session recovery is already in progress"}
	}
	defer finishRemoteControlRecovery(requestKey)
	change, found, err := recentRemoteControlSessionChange(codexHomeDir(), threadID, targetProvider)
	if err != nil {
		return map[string]any{"status": "failed", "message": "Remote Control session recovery failed: " + err.Error()}
	}
	if !found {
		return map[string]any{"status": "skipped", "message": "Remote Control session recovery is waiting for a recent openai thread"}
	}
	request := pendingRemoteControlRecovery{
		ThreadID: threadID, ProfileID: active.ID, TargetProvider: targetProvider, CreatedAt: time.Now().Unix(),
	}
	if err := enqueuePendingRemoteControlRecovery(request); err != nil {
		return map[string]any{"status": "failed", "message": "Remote Control session recovery queue failed: " + err.Error()}
	}
	rows, err := recoverRemoteControlSessionCatalog(change, targetProvider)
	if err != nil {
		return map[string]any{"status": "failed", "message": "Remote Control session catalog recovery failed: " + err.Error()}
	}
	appendDiagnosticLog("remote_control.catalog_recovered", map[string]any{"thread_id": threadID, "sqlite_rows": rows})
	return map[string]any{
		"status": "ok", "message": "Remote Control session catalog recovery complete", "thread_id": threadID,
		"sqlite_catalog_rows_inserted": rows,
	}
}

func recentRemoteControlSessionChange(home, threadID, targetProvider string) (sessionChange, bool, error) {
	change, found, err := targetedRemoteControlSessionChange(home, threadID, targetProvider)
	if err != nil {
		return sessionChange{}, false, err
	}
	if !found || !change.RewriteNeeded || providerFromSessionFirstLine(change.OriginalFirstLine) != "openai" {
		return sessionChange{}, false, nil
	}
	if info, statErr := os.Stat(change.Path); statErr != nil || time.Since(info.ModTime()) > 15*time.Minute {
		return sessionChange{}, false, nil
	}
	return change, true, nil
}

func beginRemoteControlRecovery(key string) bool {
	remoteControlRecoveryInFlight.Lock()
	defer remoteControlRecoveryInFlight.Unlock()
	if _, exists := remoteControlRecoveryInFlight.keys[key]; exists {
		return false
	}
	remoteControlRecoveryInFlight.keys[key] = struct{}{}
	return true
}

func finishRemoteControlRecovery(key string) {
	remoteControlRecoveryInFlight.Lock()
	delete(remoteControlRecoveryInFlight.keys, key)
	remoteControlRecoveryInFlight.Unlock()
}

func providerFromSessionFirstLine(line string) string {
	var record map[string]any
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.UseNumber()
	if decoder.Decode(&record) != nil {
		return ""
	}
	payload, _ := record["payload"].(map[string]any)
	return strings.TrimSpace(stringFromAny(payload["model_provider"]))
}

func enqueuePendingRemoteControlRecovery(request pendingRemoteControlRecovery) error {
	remoteControlRecoveryMu.Lock()
	defer remoteControlRecoveryMu.Unlock()
	return enqueuePendingRemoteControlRecoveryLocked(request)
}

func enqueuePendingRemoteControlRecoveryLocked(request pendingRemoteControlRecovery) error {
	path := pendingRemoteControlRecoveryPath()
	state := pendingRemoteControlRecoveryState{Version: 1}
	if err := readJSON(path, &state); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Rename(path, path+fmt.Sprintf(".corrupt-%d", time.Now().UnixNano()))
		state = pendingRemoteControlRecoveryState{Version: 1}
	}
	for _, existing := range state.Requests {
		if existing.ThreadID == request.ThreadID {
			return nil
		}
	}
	state.Requests = append(state.Requests, request)
	return atomicWriteJSON(path, state)
}

func runPendingRemoteControlRecoveries(home string) (int, error) {
	remoteControlRecoveryMu.Lock()
	defer remoteControlRecoveryMu.Unlock()
	path := pendingRemoteControlRecoveryPath()
	state := pendingRemoteControlRecoveryState{Version: 1}
	if err := readJSON(path, &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	remaining := make([]pendingRemoteControlRecovery, 0, len(state.Requests))
	completed := 0
	for _, request := range state.Requests {
		change, found, err := remoteControlSessionChange(home, request.ThreadID, request.TargetProvider)
		if err != nil || !found {
			remaining = append(remaining, request)
			continue
		}
		if err := rewriteProviderSyncSessionChange(change, false); err != nil {
			remaining = append(remaining, request)
			continue
		}
		if _, err := recoverRemoteControlSessionCatalog(change, request.TargetProvider); err != nil {
			_ = rewriteProviderSyncSessionChange(change, true)
			remaining = append(remaining, request)
			continue
		}
		completed++
	}
	if len(remaining) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return completed, err
		}
		return completed, nil
	}
	state.Requests = remaining
	return completed, atomicWriteJSON(path, state)
}

func remoteControlSessionChange(home, threadID, targetProvider string) (sessionChange, bool, error) {
	return targetedRemoteControlSessionChange(home, threadID, targetProvider)
}

// Remote-control recovery is a single-thread operation. Scanning every full
// transcript here can read many gigabytes and overlapping bridge retries make
// that cost multiply, so only metadata for the requested thread is inspected.
func targetedRemoteControlSessionChange(home, threadID, targetProvider string) (sessionChange, bool, error) {
	threadID = bareSessionID(threadID)
	if threadID == "" {
		return sessionChange{}, false, nil
	}
	paths := map[string]struct{}{}
	variants := sessionIDVariants(threadID)
	for _, dbPath := range codexSessionDBPaths(home) {
		rows, err := sqliteThreadRowsByIDs(dbPath, variants)
		if err != nil {
			return sessionChange{}, false, err
		}
		if len(rows) == 0 {
			rows, err = sqliteAutomationRunRowsByIDs(dbPath, variants)
			if err != nil {
				return sessionChange{}, false, err
			}
		}
		for _, row := range rows {
			path := normalizeRolloutPath(home, stringFromAny(row.Values["rollout_path"]))
			if path != "" && fileExists(path) {
				paths[filepath.Clean(path)] = struct{}{}
			}
		}
	}

	if len(paths) == 0 {
		for _, dirname := range []string{"sessions", "archived_sessions"} {
			root := filepath.Join(home, dirname)
			if !isDir(root) {
				continue
			}
			if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
					return nil
				}
				if rolloutFileNameMatchesAnyID(entry.Name(), map[string]bool{threadID: true}) {
					paths[filepath.Clean(path)] = struct{}{}
					return filepath.SkipAll
				}
				file, readErr := readRolloutFile(path)
				if readErr == nil && bareSessionID(file.SessionID) == threadID {
					paths[filepath.Clean(path)] = struct{}{}
					return filepath.SkipAll
				}
				return nil
			}); err != nil {
				return sessionChange{}, false, err
			}
			if len(paths) > 0 {
				break
			}
		}
	}

	candidates := make([]string, 0, len(paths))
	for path := range paths {
		candidates = append(candidates, path)
	}
	sort.Strings(candidates)
	for _, path := range candidates {
		file, err := readRolloutFile(path)
		if err != nil || bareSessionID(file.SessionID) != threadID {
			continue
		}
		return remoteControlSessionChangeFromRollout(file, targetProvider), true, nil
	}
	return sessionChange{}, false, nil
}

func remoteControlSessionChangeFromRollout(file sessionRolloutFile, targetProvider string) sessionChange {
	payload, _ := file.Record["payload"].(map[string]any)
	originalProvider := strings.TrimSpace(stringFromAny(payload["model_provider"]))
	payload["model_provider"] = targetProvider
	nextBytes, _ := json.Marshal(file.Record)
	updatedAtMS := file.UpdatedAtMs
	if info, err := os.Stat(file.Path); err == nil {
		updatedAtMS = info.ModTime().UnixMilli()
	}
	title := firstString(file.Title, file.SessionID)
	return sessionChange{
		Path: file.Path, OriginalFirstLine: file.FirstLine, NextFirstLine: string(nextBytes),
		OriginalSessionMetaLines: []string{file.FirstLine}, NextSessionMetaLines: []string{string(nextBytes)},
		Separator: file.Separator, ThreadID: file.SessionID, CWD: file.CWD,
		Source: firstString(payload["source"], payload["originator"], "vscode"), Title: title, Preview: title,
		CreatedAt: timestampMsToSeconds(file.CreatedAtMs), UpdatedAt: timestampMsToSeconds(updatedAtMS),
		CreatedAtMs: file.CreatedAtMs, UpdatedAtMs: updatedAtMS,
		Archived:   strings.Contains(filepath.ToSlash(file.Path), "/archived_sessions/"),
		CLIVersion: stringFromAny(payload["cli_version"]), SandboxPolicy: `{"type":"danger-full-access"}`,
		ApprovalMode: "never", HasUserEvent: true, RewriteNeeded: originalProvider != targetProvider,
	}
}

func recoverRemoteControlSessionCatalog(change sessionChange, targetProvider string) (int, error) {
	total := 0
	for _, path := range codexSessionDBPaths(codexHomeDir()) {
		rows, err := updateRemoteControlCatalogDB(path, change, targetProvider)
		if err != nil {
			return total, err
		}
		total += rows
	}
	return total, nil
}

func updateRemoteControlCatalogDB(path string, change sessionChange, targetProvider string) (int, error) {
	if !fileExists(path) {
		return 0, nil
	}
	db, err := openSQLite(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	columns, err := sqliteTableColumns(db, "threads")
	if err != nil || !containsString(columns, "id") {
		return 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows := 0
	if containsString(columns, "model_provider") {
		result, execErr := tx.Exec("UPDATE threads SET model_provider = ? WHERE id = ? AND COALESCE(model_provider, '') IN ('', 'openai')", targetProvider, change.ThreadID)
		if execErr != nil {
			return 0, execErr
		}
		affected, _ := result.RowsAffected()
		rows += int(affected)
	}
	var exists int
	if err := tx.QueryRow("SELECT COUNT(*) FROM threads WHERE id = ?", change.ThreadID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		copyChange := change
		copyChange.HasUserEvent = true
		inserted, insertErr := insertMissingSQLiteThreadsTx(tx, columns, targetProvider, []sessionChange{copyChange})
		if insertErr != nil {
			return 0, insertErr
		}
		rows += inserted
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return rows, nil
}
