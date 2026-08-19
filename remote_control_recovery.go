package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var remoteControlRecoveryMu sync.Mutex

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
	changes, err := collectSessionChanges(home, targetProvider)
	if err != nil {
		return sessionChange{}, false, err
	}
	for _, change := range changes {
		if bareSessionID(change.ThreadID) != bareSessionID(threadID) || !change.RewriteNeeded {
			continue
		}
		if providerFromSessionFirstLine(change.OriginalFirstLine) != "openai" {
			continue
		}
		if info, statErr := os.Stat(change.Path); statErr != nil || time.Since(info.ModTime()) > 15*time.Minute {
			continue
		}
		return change, true, nil
	}
	return sessionChange{}, false, nil
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
	changes, err := collectSessionChanges(home, targetProvider)
	if err != nil {
		return sessionChange{}, false, err
	}
	for _, change := range changes {
		if bareSessionID(change.ThreadID) == bareSessionID(threadID) {
			return change, true, nil
		}
	}
	return sessionChange{}, false, nil
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
