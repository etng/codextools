package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestListLocalSessionsPaginatesAndDeduplicatesAcrossDatabases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_SQLITE_HOME", home)
	sqliteDir := filepath.Join(home, "sqlite")
	if err := os.MkdirAll(sqliteDir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(sqliteDir, "a.db")
	second := filepath.Join(sqliteDir, "b.sqlite")
	relationOnly := filepath.Join(sqliteDir, "relations.sqlite3")
	createLocalSessionsDB(t, first, []localSessionFixture{
		{id: "t1", title: "Older duplicate", updated: 100},
		{id: "t2", title: "Second", updated: 300},
	})
	createLocalSessionsDB(t, second, []localSessionFixture{
		{id: "t1", title: "Newer duplicate", updated: 400},
	})
	db, err := openSQLite(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE automation_runs (thread_id TEXT, thread_title TEXT, source_cwd TEXT, status TEXT, updated_at INTEGER); INSERT INTO automation_runs VALUES ('t3', 'Automation', '/tmp/auto', 'archived', 200)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openSQLite(relationOnly)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE local_thread_catalog (thread_id TEXT PRIMARY KEY); INSERT INTO local_thread_catalog VALUES ('relation-only')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	firstPage, readErrors := listLocalSessions(home, 0, 2)
	if len(readErrors) != 0 {
		t.Fatalf("read errors = %#v", readErrors)
	}
	if len(firstPage.DBPaths) != 2 {
		t.Fatalf("primary DB paths = %#v", firstPage.DBPaths)
	}
	if len(firstPage.Sessions) != 2 || firstPage.Sessions[0].ID != "t1" || firstPage.Sessions[0].Title != "Newer duplicate" || firstPage.Sessions[1].ID != "t2" {
		t.Fatalf("first page = %#v", firstPage.Sessions)
	}
	if !firstPage.HasMore {
		t.Fatal("first page should report more sessions")
	}
	secondPage, readErrors := listLocalSessions(home, 2, 2)
	if len(readErrors) != 0 || len(secondPage.Sessions) != 1 || secondPage.Sessions[0].ID != "t3" || !secondPage.Sessions[0].Archived {
		t.Fatalf("second page = %#v, errors = %#v", secondPage.Sessions, readErrors)
	}
	if secondPage.HasMore {
		t.Fatal("last page should not report more sessions")
	}
}

func TestListLocalSessionsClampsLimitAndManagerRoutes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_SQLITE_HOME", home)
	path := filepath.Join(home, "sqlite", "sessions.db")
	createLocalSessionsDB(t, path, []localSessionFixture{{id: "t1", title: "One", updated: 1}})

	payload, readErrors := listLocalSessions(home, -1, 1000)
	if len(readErrors) != 0 || payload.Offset != 0 || payload.Limit != maxLocalSessionsPageSize {
		t.Fatalf("clamped payload = %#v, errors = %#v", payload, readErrors)
	}
	manager := &server{}
	listed := manager.dispatch(testContext(), "list_local_sessions", map[string]any{"request": map[string]any{"offset": 0, "limit": 50}})
	if listed["status"] != "ok" || len(listed["sessions"].([]localSession)) != 1 {
		t.Fatalf("list command = %#v", listed)
	}
	preview := manager.dispatch(testContext(), "preview_session_index_cleanup", map[string]any{})
	if preview["status"] != "ok" || preview["snapshotSha256"] == "" {
		t.Fatalf("preview command = %#v", preview)
	}
}

type localSessionFixture struct {
	id      string
	title   string
	updated int64
}

func createLocalSessionsDB(t *testing.T, path string, fixtures []localSessionFixture) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT, cwd TEXT, model_provider TEXT, archived INTEGER, updated_at_ms INTEGER, rollout_path TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		if _, err := db.Exec(`INSERT INTO threads VALUES (?, ?, '/tmp/project', 'openai', 0, ?, '')`, fixture.id, fixture.title, fixture.updated); err != nil {
			t.Fatal(err)
		}
	}
}

func testContext() context.Context { return context.Background() }
