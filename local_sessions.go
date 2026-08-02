package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultLocalSessionsPageSize = 50
	maxLocalSessionsPageSize     = 100
)

type localSession struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	CWD           string `json:"cwd"`
	ModelProvider string `json:"modelProvider"`
	Archived      bool   `json:"archived"`
	UpdatedAtMS   int64  `json:"updatedAtMs"`
	RolloutPath   string `json:"rolloutPath"`
	DBPath        string `json:"dbPath"`
}

type localSessionsPayload struct {
	DBPath   string         `json:"dbPath"`
	DBPaths  []string       `json:"dbPaths"`
	Sessions []localSession `json:"sessions"`
	Offset   int            `json:"offset"`
	Limit    int            `json:"limit"`
	HasMore  bool           `json:"hasMore"`
}

func (s *server) listLocalSessions(args map[string]any) commandResult {
	request := mapArgOrSelf(args)
	offset := intArg(request, "offset", 0)
	limit := intArg(request, "limit", defaultLocalSessionsPageSize)
	payload, readErrors := listLocalSessions(codexHomeDir(), offset, limit)
	result := map[string]any{
		"dbPath":   payload.DBPath,
		"dbPaths":  payload.DBPaths,
		"sessions": payload.Sessions,
		"offset":   payload.Offset,
		"limit":    payload.Limit,
		"hasMore":  payload.HasMore,
	}
	page := payload.Offset/payload.Limit + 1
	if len(readErrors) > 0 {
		return failed("读取部分本地会话失败："+strings.Join(readErrors, "; "), result)
	}
	return ok(fmt.Sprintf("已读取第 %d 页，共 %d 个本地会话。", page, len(payload.Sessions)), result)
}

func (s *server) deleteLocalSession(args map[string]any) commandResult {
	request := mapArgOrSelf(args)
	sessionID := firstString(request["sessionId"], request["session_id"])
	result := handleSessionDataRoute("/delete", map[string]any{
		"session_id": sessionID,
		"title":      stringArg(request, "title"),
		"db_path":    stringArg(request, "dbPath"),
	})
	if status := stringFromAny(result["status"]); status == "ok" || status == "local_deleted" {
		result["status"] = "ok"
		result["message"] = "本地会话已删除，并已创建恢复备份。"
	}
	return commandResult(result)
}

func (s *server) previewSessionIndexCleanup() commandResult {
	preview, err := previewSessionIndexCleanup(codexHomeDir())
	if err != nil {
		return failed("预览失效任务索引失败："+err.Error(), map[string]any{})
	}
	return ok(fmt.Sprintf("发现 %d 条仅存在于任务索引中的候选记录。", len(preview.Candidates)), map[string]any{
		"snapshotSha256": preview.SnapshotSHA256,
		"candidates":     preview.Candidates,
	})
}

func (s *server) applySessionIndexCleanup(args map[string]any) commandResult {
	request := mapArgOrSelf(args)
	result, err := applySessionIndexCleanup(
		codexHomeDir(),
		stringArg(request, "snapshotSha256"),
		stringSliceArg(request, "threadIds"),
		true,
	)
	if err == nil {
		return ok(fmt.Sprintf("已清理 %d 条失效任务索引；原索引已完整备份。", result.PrunedEntries), map[string]any{
			"prunedEntries": result.PrunedEntries,
			"backupDir":     result.BackupDir,
		})
	}
	payload := map[string]any{}
	var applyErr *sessionIndexCleanupApplyError
	if errors.As(err, &applyErr) && applyErr.BackupDir != "" {
		payload["backupDir"] = applyErr.BackupDir
	}
	return failed("清理失效任务索引失败："+err.Error(), payload)
}

func listLocalSessions(home string, offset, limit int) (localSessionsPayload, []string) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = defaultLocalSessionsPageSize
	}
	if limit > maxLocalSessionsPageSize {
		limit = maxLocalSessionsPageSize
	}
	fetchLimit := offset + limit + 1
	dbPaths := codexPrimarySessionDBPaths(home)
	all := []localSession{}
	errors := []string{}
	for _, path := range dbPaths {
		items, err := listLocalSessionsFromDB(path, fetchLimit)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		all = append(all, items...)
	}
	sort.SliceStable(all, func(left, right int) bool {
		if all[left].UpdatedAtMS != all[right].UpdatedAtMS {
			return all[left].UpdatedAtMS > all[right].UpdatedAtMS
		}
		return all[left].ID > all[right].ID
	})
	seen := map[string]bool{}
	deduplicated := make([]localSession, 0, len(all))
	for _, session := range all {
		if session.ID == "" || seen[session.ID] {
			continue
		}
		seen[session.ID] = true
		deduplicated = append(deduplicated, session)
	}
	hasMore := len(deduplicated) > offset+limit
	start := offset
	if start > len(deduplicated) {
		start = len(deduplicated)
	}
	end := start + limit
	if end > len(deduplicated) {
		end = len(deduplicated)
	}
	page := append([]localSession{}, deduplicated[start:end]...)
	primary := ""
	if len(dbPaths) > 0 {
		primary = dbPaths[0]
	}
	return localSessionsPayload{
		DBPath:   primary,
		DBPaths:  dbPaths,
		Sessions: page,
		Offset:   offset,
		Limit:    limit,
		HasMore:  hasMore,
	}, errors
}

func codexPrimarySessionDBPaths(home string) []string {
	paths := codexThreadReferenceDBPaths(home)
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if sqlitePathHasTable(path, "threads") || sqlitePathHasTable(path, "automation_runs") {
			out = append(out, filepath.Clean(path))
		}
	}
	return out
}

func listLocalSessionsFromDB(path string, limit int) ([]localSession, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	result := []localSession{}
	threadsColumns, err := sqliteTableColumns(db, "threads")
	if err != nil {
		return nil, err
	}
	if containsString(threadsColumns, "id") {
		rows, err := querySessionSQLiteRows(db, localSessionsQuery("threads", threadsColumns, limit))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if session, ok := localSessionFromThreadRow(path, row); ok {
				result = append(result, session)
			}
		}
	}
	automationColumns, err := sqliteTableColumns(db, "automation_runs")
	if err != nil {
		return nil, err
	}
	if containsString(automationColumns, "thread_id") {
		rows, err := querySessionSQLiteRows(db, localSessionsQuery("automation_runs", automationColumns, limit))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if session, ok := localSessionFromAutomationRow(path, row); ok {
				result = append(result, session)
			}
		}
	}
	return result, nil
}

func localSessionsQuery(table string, columns []string, limit int) string {
	idColumn := "id"
	if table == "automation_runs" {
		idColumn = "thread_id"
	}
	orderColumn := ""
	for _, candidate := range []string{"updated_at_ms", "updated_at", "created_at_ms", "created_at"} {
		if containsString(columns, candidate) {
			orderColumn = candidate
			break
		}
	}
	order := quoteSQLiteIdentifier(idColumn) + " DESC"
	if orderColumn != "" {
		order = "COALESCE(" + quoteSQLiteIdentifier(orderColumn) + ", 0) DESC, " + order
	}
	return fmt.Sprintf("SELECT * FROM %s WHERE COALESCE(%s, '') <> '' ORDER BY %s LIMIT %d", quoteSQLiteIdentifier(table), quoteSQLiteIdentifier(idColumn), order, limit)
}

func localSessionFromThreadRow(path string, row sessionSQLiteRow) (localSession, bool) {
	id := strings.TrimSpace(stringFromAny(row.Values["id"]))
	if id == "" {
		return localSession{}, false
	}
	updated := timestampMsFromAny(row.Values["updated_at_ms"])
	if updated == 0 {
		updated = timestampMsFromAny(row.Values["updated_at"])
	}
	if updated == 0 {
		updated = timestampMsFromAny(row.Values["created_at_ms"])
	}
	if updated == 0 {
		updated = timestampMsFromAny(row.Values["created_at"])
	}
	if updated == 0 {
		updated = uuidV7TimestampMs(id)
	}
	return localSession{
		ID:            id,
		Title:         firstString(row.Values["title"], row.Values["thread_title"]),
		CWD:           firstString(row.Values["cwd"], row.Values["source_cwd"]),
		ModelProvider: firstString(row.Values["model_provider"]),
		Archived:      boolFromAny(row.Values["archived"]),
		UpdatedAtMS:   updated,
		RolloutPath:   firstString(row.Values["rollout_path"]),
		DBPath:        path,
	}, true
}

func localSessionFromAutomationRow(path string, row sessionSQLiteRow) (localSession, bool) {
	id := strings.TrimSpace(stringFromAny(row.Values["thread_id"]))
	if id == "" {
		return localSession{}, false
	}
	updated := timestampMsFromAny(row.Values["updated_at_ms"])
	if updated == 0 {
		updated = timestampMsFromAny(row.Values["updated_at"])
	}
	if updated == 0 {
		updated = timestampMsFromAny(row.Values["created_at_ms"])
	}
	if updated == 0 {
		updated = timestampMsFromAny(row.Values["created_at"])
	}
	status := strings.TrimSpace(stringFromAny(row.Values["status"]))
	return localSession{
		ID:          id,
		Title:       firstString(row.Values["thread_title"], row.Values["title"]),
		CWD:         firstString(row.Values["source_cwd"], row.Values["cwd"]),
		Archived:    strings.EqualFold(status, "archived"),
		UpdatedAtMS: updated,
		DBPath:      path,
	}, true
}
