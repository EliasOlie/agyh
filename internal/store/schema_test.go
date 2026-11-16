package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/model"
)

// v2Schema is the layout shipped up to v0.2.1: FTS rows keyed by UNINDEXED agent/id.
const v2Schema = `
CREATE TABLE sessions (
	agent TEXT NOT NULL, id TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', preview TEXT NOT NULL DEFAULT '',
	workspace TEXT NOT NULL DEFAULT '', started_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0,
	messages INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT '', parent TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '', mtime INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (agent, id));
CREATE INDEX sessions_updated ON sessions(updated_at DESC);
CREATE INDEX sessions_path ON sessions(path);
CREATE VIRTUAL TABLE sessions_fts USING fts5(agent UNINDEXED, id UNINDEXED, title, workspace, body,
	tokenize = 'unicode61 remove_diacritics 2', prefix = '2 3 4');
CREATE TABLE projects (name TEXT PRIMARY KEY, description TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE TABLE project_sessions (
	project TEXT NOT NULL REFERENCES projects(name) ON DELETE CASCADE ON UPDATE CASCADE,
	agent TEXT NOT NULL, id TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', added_at INTEGER NOT NULL,
	PRIMARY KEY (project, agent, id));
PRAGMA user_version = 2;
`

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestMigrateV2InPlaceKeepsSessionsTextAndProjects(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	now := time.Now().Unix()

	db := rawDB(t, path)
	exec(t, db, v2Schema)
	// One agent session and one restored from the vault: a sync never re-reads the latter.
	for _, r := range []struct{ agent, id, title, path, body string }{
		{"claude", "aaaa1111", "Auth refactor", "/p/aaaa1111", "rotação de credenciais"},
		{"agy", "bbbb2222", "Pix no PDV", "archive:/arch/bbbb2222.json", "webhook idempotente"},
	} {
		exec(t, db, `INSERT INTO sessions (agent, id, title, path, updated_at, mtime) VALUES (?, ?, ?, ?, ?, 42)`,
			r.agent, r.id, r.title, r.path, now)
		exec(t, db, `INSERT INTO sessions_fts (agent, id, title, workspace, body) VALUES (?, ?, ?, '', ?)`,
			r.agent, r.id, r.title, r.body)
	}
	exec(t, db, `INSERT INTO projects (name, created_at) VALUES ('pagamentos', ?)`, now)
	exec(t, db, `INSERT INTO project_sessions (project, agent, id, note, added_at) VALUES ('pagamentos', 'agy', 'bbbb2222', 'pix', ?)`, now)
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var v int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d, %v", v, err)
	}
	// Text came along: no re-parse needed, archive sessions stay searchable.
	for q, want := range map[string]string{"credenciais": "aaaa1111", "idempotente": "bbbb2222", "pdv": "bbbb2222"} {
		hits, err := st.Search(ctx, q, Filter{})
		if err != nil || len(hits) != 1 || hits[0].ID != want {
			t.Fatalf("search %q after migration: %+v %v", q, hits, err)
		}
	}
	// mtimes kept, so the next sync does not re-read every agent file.
	if m, _ := st.Mtimes(ctx, "claude"); m["/p/aaaa1111"] != 42 {
		t.Fatalf("mtime lost: %v", m)
	}
	hits, _ := st.List(ctx, Filter{Project: "pagamentos"})
	if len(hits) != 1 || hits[0].ID != "bbbb2222" || !slices.Equal(hits[0].Projects, []string{"pagamentos"}) {
		t.Fatalf("project membership lost: %+v", hits)
	}
	if notes, _ := st.Notes(ctx, "pagamentos"); notes["agy:bbbb2222"] != "pix" {
		t.Fatalf("note lost: %v", notes)
	}
	for _, old := range []string{"sessions_v2", "sessions_fts_v2"} {
		var n int
		st.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, old).Scan(&n)
		if n != 0 {
			t.Fatalf("%s left behind", old)
		}
	}
	assertIndexConsistent(t, st)

	// Reopening is a no-op.
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if hits, _ := st.Search(ctx, "credenciais", Filter{}); len(hits) != 1 {
		t.Fatal("second open changed the index")
	}
}

func TestMigrateFromV1DropsDerivedTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db := rawDB(t, path)
	exec(t, db, strings.Replace(v2Schema, "PRAGMA user_version = 2;", "", 1))
	exec(t, db, `INSERT INTO sessions (agent, id) VALUES ('claude', 'old')`)
	exec(t, db, `INSERT INTO projects (name, created_at) VALUES ('keep', 1)`)
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if c, _ := st.Count(context.Background()); len(c) != 0 {
		t.Fatalf("v1 sessions should be dropped and re-read: %v", c)
	}
	if ps, _ := st.Projects(context.Background()); len(ps) != 1 || ps[0].Name != "keep" {
		t.Fatalf("projects must survive: %+v", ps)
	}
}

// assertIndexConsistent checks that every session has exactly one FTS row and
// that no FTS row is orphaned.
func assertIndexConsistent(t *testing.T, st *Store) {
	t.Helper()
	var sessions, fts, orphans, missing int
	st.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
	st.db.QueryRow(`SELECT count(*) FROM sessions_fts`).Scan(&fts)
	st.db.QueryRow(`SELECT count(*) FROM sessions_fts f WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.rid = f.rowid)`).Scan(&orphans)
	st.db.QueryRow(`SELECT count(*) FROM sessions s WHERE NOT EXISTS (SELECT 1 FROM sessions_fts f WHERE f.rowid = s.rid)`).Scan(&missing)
	if sessions != fts || orphans != 0 || missing != 0 {
		t.Fatalf("index out of sync: %d sessions, %d fts rows, %d orphans, %d missing", sessions, fts, orphans, missing)
	}
}

func TestReupsertAndDeleteKeepIndexConsistent(t *testing.T) {
	st := open(t)
	seed(t, st)
	ctx := context.Background()
	s := sess("claude", "aaaa1111", "Auth refactor v2", "/w/api", time.Minute)
	// Updating replaces the text instead of adding a second FTS row.
	if err := st.Upsert(ctx, s, []model.Message{{Role: "user", Text: "agora com mTLS"}}); err != nil {
		t.Fatal(err)
	}
	assertIndexConsistent(t, st)
	if hits, _ := st.Search(ctx, "credenciais", Filter{}); len(hits) != 0 {
		t.Fatal("stale text still searchable after update")
	}
	if hits, _ := st.Search(ctx, "mtls", Filter{}); len(hits) != 1 || hits[0].Title != "Auth refactor v2" {
		t.Fatalf("new text not searchable: %+v", hits)
	}

	// Archive and path-less sessions are never removed.
	arch := sess("claude", "cccc3333", "Restored", "/w/x", time.Hour)
	arch.Path = "archive:/arch/cccc3333.json"
	noPath := sess("claude", "dddd4444", "No path", "/w/x", time.Hour)
	noPath.Path = ""
	st.UpsertMany(ctx, []Item{{Session: arch}, {Session: noPath}})
	n, err := st.DeleteMissing(ctx, "claude", map[string]bool{})
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	if c, _ := st.Count(ctx); c["claude"] != 2 {
		t.Fatalf("archive or path-less session removed: %v", c)
	}
	assertIndexConsistent(t, st)
	if n, _ := st.DeleteMissing(ctx, "claude", map[string]bool{}); n != 0 {
		t.Fatalf("second DeleteMissing removed %d", n)
	}
}

func TestProjectsAttachedSorted(t *testing.T) {
	st := open(t)
	seed(t, st)
	ctx := context.Background()
	a, _ := st.Resolve(ctx, "aaaa1111")
	for _, p := range []string{"zeta", "alfa", "meio"} {
		if err := st.AddToProject(ctx, p, a, ""); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"alfa", "meio", "zeta"}
	list, _ := st.List(ctx, Filter{Agent: "claude"})
	found, _ := st.Search(ctx, "credenciais", Filter{})
	if len(list) != 1 || !slices.Equal(list[0].Projects, want) || len(found) != 1 || !slices.Equal(found[0].Projects, want) {
		t.Fatalf("projects: list %+v search %+v", list, found)
	}
	if other, _ := st.List(ctx, Filter{Agent: "codex"}); len(other) != 1 || other[0].Projects != nil {
		t.Fatalf("session without projects: %+v", other)
	}
}

// The quadratic regression: FTS writes must look rows up by rowid, never scan.
func TestFTSWritesDoNotScan(t *testing.T) {
	st := open(t)
	for _, q := range []string{
		`DELETE FROM sessions_fts WHERE rowid = 1`,
		`SELECT s.id FROM sessions_fts f JOIN sessions s ON s.rid = f.rowid WHERE sessions_fts MATCH 'x'`,
		`SELECT project FROM project_sessions p WHERE p.agent = 'a' AND p.id = 'b'`,
	} {
		rows, err := st.db.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			rows.Scan(&id, &parent, &notused, &detail)
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "; ")
		// FTS5 reports a rowid lookup as "INDEX 0:=" style constraints; a bare
		// "INDEX 0:" (no constraint) is a full scan.
		for _, step := range plan {
			if strings.HasSuffix(step, "VIRTUAL TABLE INDEX 0:") || (strings.HasPrefix(step, "SCAN ") && !strings.Contains(step, "VIRTUAL TABLE")) {
				t.Errorf("%s\n  plan scans: %s", q, joined)
			}
		}
	}
}

func benchItems(n int) []Item {
	items := make([]Item, n)
	text := strings.Repeat("palavra rotação credencial webhook ", 200)
	for i := range items {
		id := fmt.Sprintf("id-%06d", i)
		items[i] = Item{Session: model.Session{Agent: "claude", ID: id, Title: "t", Path: "/p/" + id, UpdatedAt: time.Now()},
			Messages: []model.Message{{Role: "user", Text: text}}}
	}
	return items
}

// BenchmarkIndex measures a first index of 2,000 sessions in batches of 64
// (what index.Sync does), then removing all of them.
func BenchmarkIndex(b *testing.B) {
	ctx := context.Background()
	items := benchItems(2000)
	for b.Loop() {
		st, err := Open(filepath.Join(b.TempDir(), "bench.db"))
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < len(items); i += 64 {
			if err := st.UpsertMany(ctx, items[i:min(i+64, len(items))]); err != nil {
				b.Fatal(err)
			}
		}
		if n, err := st.DeleteMissing(ctx, "claude", nil); err != nil || n != len(items) {
			b.Fatal(n, err)
		}
		st.Close()
	}
}
