package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/model"
)

func TestProjectColorColumnIsAddedToOldIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, _ := sql.Open("sqlite", "file:"+path)
	db.Exec(`CREATE TABLE projects (name TEXT PRIMARY KEY, description TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
		INSERT INTO projects (name, created_at) VALUES ('old', 1); PRAGMA user_version = 3;`)
	db.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.SetProjectColor(ctx, "old", "green"); err != nil {
		t.Fatal(err)
	}
	ps, _ := st.Projects(ctx)
	if len(ps) != 1 || ps[0].Color != "green" {
		t.Fatalf("projects: %+v", ps)
	}
	if m, _ := st.ProjectColors(ctx); m["old"] != "green" {
		t.Fatalf("colors: %v", m)
	}
	if err := st.SetProjectColor(ctx, "missing", "red"); err != ErrNotFound {
		t.Fatalf("missing project: %v", err)
	}
}

func TestPendingProjectJoinsTheNewSession(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	start := time.Now().Add(-10 * time.Minute)
	mk := func(id, ws string, startedAgo time.Duration) {
		s := model.Session{Agent: "claude", ID: id, Title: id, Workspace: ws,
			StartedAt: time.Now().Add(-startedAgo), UpdatedAt: time.Now(), Path: "/p/" + id, ModTime: time.Now()}
		if err := st.Upsert(ctx, s, nil); err != nil {
			t.Fatal(err)
		}
	}
	mk("older", "/w/api", time.Hour)       // started before the request: not it
	mk("elsewhere", "/w/pdv", time.Minute) // other workspace: not it
	if err := st.AddPendingProject(ctx, "claude", "/w/api/", "pagamentos", start); err != nil {
		t.Fatal(err)
	}
	if n, err := st.ResolvePending(ctx); err != nil || n != 0 {
		t.Fatalf("nothing to assign yet: %d %v", n, err)
	}
	mk("fresh", "/w/api", time.Minute)
	mk("later", "/w/api", 30*time.Second)
	if n, err := st.ResolvePending(ctx); err != nil || n != 1 {
		t.Fatalf("assign: %d %v", n, err)
	}
	hits, _ := st.List(ctx, Filter{Project: "pagamentos"})
	if len(hits) != 1 || hits[0].ID != "fresh" {
		t.Fatalf("the first session started after the request joins: %+v", hits)
	}
	if n, _ := st.ResolvePending(ctx); n != 0 {
		t.Fatal("a pending request is used once")
	}

	// Requests older than a day expire.
	st.AddPendingProject(ctx, "claude", "/w/pdv", "old", time.Now().Add(-48*time.Hour))
	if n, _ := st.ResolvePending(ctx); n != 0 {
		t.Fatal("expired request assigned")
	}
	if ps, _ := st.Projects(ctx); !slices.ContainsFunc(ps, func(p Project) bool { return p.Name == "old" && p.Sessions == 0 }) {
		t.Fatalf("projects: %+v", ps)
	}
}
