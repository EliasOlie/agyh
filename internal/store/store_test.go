package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/model"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sess(agent, id, title, ws string, ago time.Duration) model.Session {
	now := time.Now()
	return model.Session{Agent: agent, ID: id, Title: title, Workspace: ws,
		StartedAt: now.Add(-ago - time.Hour), UpdatedAt: now.Add(-ago), Path: "/p/" + id, ModTime: now}
}

func seed(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	items := []Item{
		{sess("claude", "aaaa1111", "Auth refactor", "/w/api", time.Hour), []model.Message{
			{Role: "user", Text: "Implementa rotação de credenciais"}, {Role: "assistant", Text: "Feito com refresh tokens."}}},
		{sess("agy", "bbbb2222", "Pix no PDV", "/w/pdv", 2*time.Hour), []model.Message{
			{Role: "user", Text: "integrar pix com webhook idempotente"}}},
		{sess("codex", "aaaa9999", "Queue retries", "/w/queue", 3*time.Hour), []model.Message{
			{Role: "user", Text: "add exponential backoff"}}},
	}
	if err := st.UpsertMany(ctx, items); err != nil {
		t.Fatal(err)
	}
}

func TestSearchIgnoresAccentsAndCaseWithPrefix(t *testing.T) {
	st := open(t)
	seed(t, st)
	ctx := context.Background()
	for _, q := range []string{"rotacao", "ROTAÇÃO", "credenc", "rotação credenciais"} {
		hits, err := st.Search(ctx, q, Filter{})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(hits) != 1 || hits[0].ID != "aaaa1111" {
			t.Fatalf("%q: got %+v", q, hits)
		}
		if hits[0].Snippet == "" {
			t.Fatalf("%q: expected a snippet", q)
		}
	}
	hits, _ := st.Search(ctx, "webhook", Filter{Agent: "claude"})
	if len(hits) != 0 {
		t.Fatalf("agent filter ignored: %+v", hits)
	}
}

func TestRecencyTiersOutrankOlderMatches(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	// The old session matches better (term in the title), but the recent one wins its tier.
	old := sess("claude", "old00001", "deploy pipeline", "/w", 120*24*time.Hour)
	recent := sess("claude", "new00001", "misc", "/w", 2*24*time.Hour)
	st.Upsert(ctx, old, []model.Message{{Role: "user", Text: "deploy deploy deploy"}})
	st.Upsert(ctx, recent, []model.Message{{Role: "user", Text: "fix the deploy step"}})
	hits, err := st.Search(ctx, "deploy", Filter{})
	if err != nil || len(hits) != 2 || hits[0].ID != "new00001" {
		t.Fatalf("recent session should rank first: %+v %v", hits, err)
	}
}

func TestSearchInputCannotInjectFTSSyntax(t *testing.T) {
	st := open(t)
	seed(t, st)
	for _, q := range []string{`"unbalanced`, `NEAR(a b)`, `title:x OR *`, `-`, `a AND (`} {
		if _, err := st.Search(context.Background(), q, Filter{}); err != nil {
			t.Fatalf("query %q should be safe, got %v", q, err)
		}
	}
	if got := FTSQuery(`foo "bar" baz*`); got != `"foo"* "bar"* "baz"*` {
		t.Fatalf("FTSQuery = %s", got)
	}
	if got := FTSQuery("a pix e o"); got != `"pix"*` {
		t.Fatalf("one-letter words must be dropped, got %s", got)
	}
	// A query made only of one-letter words lists instead of scanning every document.
	if hits, err := st.Search(context.Background(), "a", Filter{}); err != nil || len(hits) != 3 {
		t.Fatalf("single letter: %d hits, %v", len(hits), err)
	}
}

func TestListOrderAndUpsertReplacesBody(t *testing.T) {
	st := open(t)
	seed(t, st)
	ctx := context.Background()
	hits, err := st.List(ctx, Filter{})
	if err != nil || len(hits) != 3 || hits[0].ID != "aaaa1111" || hits[2].ID != "aaaa9999" {
		t.Fatalf("list order: %+v %v", hits, err)
	}
	s := sess("claude", "aaaa1111", "Auth refactor", "/w/api", 0)
	if err := st.Upsert(ctx, s, []model.Message{{Role: "user", Text: "totally different"}}); err != nil {
		t.Fatal(err)
	}
	if hits, _ := st.Search(ctx, "credenciais", Filter{}); len(hits) != 0 {
		t.Fatalf("old body still indexed: %+v", hits)
	}
	if hits, _ := st.Search(ctx, "different", Filter{}); len(hits) != 1 {
		t.Fatalf("new body not indexed: %+v", hits)
	}
}

func TestResolve(t *testing.T) {
	st := open(t)
	seed(t, st)
	ctx := context.Background()
	if s, err := st.Resolve(ctx, "bbbb"); err != nil || s.ID != "bbbb2222" {
		t.Fatalf("prefix: %+v %v", s, err)
	}
	var amb *ErrAmbiguous
	if _, err := st.Resolve(ctx, "aaaa"); !errors.As(err, &amb) || len(amb.Matches) != 2 {
		t.Fatalf("expected ambiguity, got %v", err)
	}
	if s, err := st.Resolve(ctx, "codex:aaaa"); err != nil || s.Agent != "codex" {
		t.Fatalf("agent-qualified: %+v %v", s, err)
	}
	if _, err := st.Resolve(ctx, "zzzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if _, err := st.Resolve(ctx, "ab"); err == nil {
		t.Fatal("short ids must be rejected")
	}
}

func TestProjects(t *testing.T) {
	st := open(t)
	seed(t, st)
	ctx := context.Background()
	a, _ := st.Resolve(ctx, "aaaa1111")
	b, _ := st.Resolve(ctx, "bbbb2222")
	if err := st.AddToProject(ctx, "pagamentos", a, "auth base"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddToProject(ctx, "pagamentos", b, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddToProject(ctx, "pagamentos", b, ""); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := st.AddToProject(ctx, "bad name!", a, ""); err == nil {
		t.Fatal("invalid project name accepted")
	}
	ps, err := st.Projects(ctx)
	if err != nil || len(ps) != 1 || ps[0].Sessions != 2 {
		t.Fatalf("projects: %+v %v", ps, err)
	}
	hits, _ := st.List(ctx, Filter{Project: "pagamentos"})
	if len(hits) != 2 || len(hits[0].Projects) != 1 {
		t.Fatalf("project filter: %+v", hits)
	}
	notes, _ := st.Notes(ctx, "pagamentos")
	if notes["claude:aaaa1111"] != "auth base" {
		t.Fatalf("note lost: %v", notes)
	}
	if err := st.RenameProject(ctx, "pagamentos", "pix"); err != nil {
		t.Fatal(err)
	}
	if hits, _ := st.List(ctx, Filter{Project: "pix"}); len(hits) != 2 {
		t.Fatalf("rename did not cascade: %+v", hits)
	}
	if err := st.RemoveFromProject(ctx, "pix", b); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProject(ctx, "pix"); err != nil {
		t.Fatal(err)
	}
	if ps, _ := st.Projects(ctx); len(ps) != 0 {
		t.Fatalf("project not deleted: %+v", ps)
	}
	if hits, _ := st.List(ctx, Filter{}); len(hits) != 3 {
		t.Fatal("deleting a project must not delete sessions")
	}
}

func TestDeleteMissing(t *testing.T) {
	st := open(t)
	seed(t, st)
	ctx := context.Background()
	n, err := st.DeleteMissing(ctx, "claude", map[string]bool{})
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	if hits, _ := st.Search(ctx, "credenciais", Filter{}); len(hits) != 0 {
		t.Fatal("fts row left behind")
	}
}
