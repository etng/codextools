package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func codexSQLiteHome(home string) string {
	if value := strings.TrimSpace(os.Getenv("CODEX_SQLITE_HOME")); value != "" {
		return filepath.Clean(os.ExpandEnv(value))
	}
	return filepath.Clean(home)
}

func codexSessionDBPaths(home string) []string {
	sqliteHome := codexSQLiteHome(home)
	paths := make([]string, 0)
	seen := map[string]bool{}
	add := func(path string) {
		path = filepath.Clean(path)
		if path == "" || seen[path] || !fileExists(path) {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}
	sqliteDir := filepath.Join(sqliteHome, "sqlite")
	if entries, err := os.ReadDir(sqliteDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			lower := strings.ToLower(entry.Name())
			if strings.HasSuffix(lower, ".db") || strings.HasSuffix(lower, ".sqlite") {
				add(filepath.Join(sqliteDir, entry.Name()))
			}
		}
	}
	sort.Strings(paths)
	add(filepath.Join(sqliteHome, "state_5.sqlite"))
	return paths
}

func codexPreferredSessionDBPath(home string) string {
	paths := codexSessionDBPaths(home)
	for _, path := range paths {
		if sqlitePathHasTable(path, "threads") {
			return path
		}
	}
	return filepath.Join(codexSQLiteHome(home), "state_5.sqlite")
}

func sqlitePathHasTable(path, table string) bool {
	if !fileExists(path) {
		return false
	}
	db, err := openSQLite(path)
	if err != nil {
		return false
	}
	defer db.Close()
	columns, err := sqliteTableColumns(db, table)
	return err == nil && len(columns) > 0
}

func codexLogsDBPath(home string) string {
	return filepath.Join(codexSQLiteHome(home), "logs_2.sqlite")
}

func pathWithin(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}
