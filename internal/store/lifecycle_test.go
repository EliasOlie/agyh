package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestProjectKeyAndSimilarNames(t *testing.T) {
	for _, pair := range [][2]string{{"conecta-me", "conectame"}, {"Conecta_Me", "conectame"}, {"Pagamentos", "pagamentos"}, {"ação", "acao"}} {
		if ProjectKey(pair[0]) != ProjectKey(pair[1]) {
			t.Errorf("%q and %q should look alike", pair[0], pair[1])
		}
	}
	st := open(t)
	ctx := context.Background()
	st.CreateProject(ctx, "conecta-me", "")
	var sim *ErrSimilarProject
	if err := st.CheckNewProject(ctx, "conectame"); !errors.As(err, &sim) || sim.Existing != "conecta-me" {
		t.Fatalf("similar: %v", err)
	}
	if err := st.CheckNewProject(ctx, "conecta-me"); err != nil {
		t.Fatalf("an existing name is fine: %v", err)
	}
	if err := st.CheckNewProject(ctx, "fiscal"); err != nil {
		t.Fatalf("a different name is fine: %v", err)
	}
}

func TestMergeKeepsSessionsAndNotes(t *testing.T) {
	st := open(t)
	seed(t, st)
	ctx := context.Background()
	a, _ := st.Resolve(ctx, "aaaa1111")
	b, _ := st.Resolve(ctx, "bbbb2222")
	st.AddToProject(ctx, "conectame", a, "from dup")
	st.AddToProject(ctx, "conectame", b, "")
	st.AddToProject(ctx, "conecta-me", a, "kept")
	st.AddPendingProject(ctx, "claude", "/w/x", "conectame", time.Now())

	n, err := st.MergeProjects(ctx, "conectame", "conecta-me")
	if err != nil || n != 2 {
		t.Fatalf("merge: %d %v", n, err)
	}
	hits, _ := st.List(ctx, Filter{Project: "conecta-me", IncludeSubs: true})
	if len(hits) != 2 {
		t.Fatalf("both sessions in the target: %+v", hits)
	}
	if notes, _ := st.Notes(ctx, "conecta-me"); notes["claude:aaaa1111"] != "kept" {
		t.Fatalf("the target's note wins: %v", notes)
	}
	ps, _ := st.Projects(ctx)
	for _, p := range ps {
		if p.Name == "conectame" {
			t.Fatal("source project not deleted")
		}
	}
	if c, _ := st.Count(ctx); c["claude"]+c["agy"]+c["codex"] != 3 {
		t.Fatalf("no session may be deleted: %v", c)
	}
	var pending string
	st.db.QueryRow(`SELECT project FROM pending_projects`).Scan(&pending)
	if pending != "conecta-me" {
		t.Fatalf("pending sessions follow the merge: %q", pending)
	}
	if tomb, _ := st.LocalTombstones(ctx); tomb.Projects["conectame"].IsZero() {
		t.Fatal("merge must leave a tombstone for the vault")
	}
	if _, err := st.MergeProjects(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing source: %v", err)
	}
	if _, err := st.MergeProjects(ctx, "conecta-me", "conecta-me"); err == nil {
		t.Fatal("self merge accepted")
	}
}

func TestTombstonesBlockVaultRevivalNewestWins(t *testing.T) {
	st := open(t)
	seed(t, st)
	ctx := context.Background()
	a, _ := st.Resolve(ctx, "aaaa1111")
	st.AddToProject(ctx, "old", a, "")
	st.DeleteProject(ctx, "old")
	if c, _ := st.Count(ctx); c["claude"] != 1 {
		t.Fatal("deleting a project must keep its sessions")
	}
	if added, _ := st.RestoreMembership(ctx, "old", a, ""); added {
		t.Fatal("the vault revived a deleted project")
	}

	st.AddToProject(ctx, "keep", a, "")
	st.RemoveFromProject(ctx, "keep", a)
	if added, _ := st.RestoreMembership(ctx, "keep", a, ""); added {
		t.Fatal("the vault revived a removed membership")
	}
	st.AddToProject(ctx, "keep", a, "") // the user adds it back: newer wins
	if tomb, _ := st.LocalTombstones(ctx); !tomb.Memberships[MembershipKey("keep", "claude", "aaaa1111")].IsZero() {
		t.Fatal("re-adding must clear the membership tombstone")
	}

	// Remote tombstones: an older local project is deleted; a newer one is kept and reported revived.
	b, _ := st.Resolve(ctx, "bbbb2222")
	st.AddToProject(ctx, "remote-gone", b, "")
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	revived, err := st.ApplyTombstones(ctx, Tombstones{Projects: map[string]time.Time{"remote-gone": future, "keep": past}})
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := st.Projects(ctx)
	names := map[string]bool{}
	for _, p := range ps {
		names[p.Name] = true
	}
	if names["remote-gone"] || !names["keep"] || revived.Projects["keep"].IsZero() {
		t.Fatalf("newest wins: projects %v revived %v", names, revived.Projects)
	}
	if added, _ := st.RestoreMembership(ctx, "remote-gone", b, ""); added {
		t.Fatal("a remote deletion must also block revival here")
	}

	// A shared tombstone that the vault dropped was revived elsewhere: cleared here.
	st.MarkTombstonesShared(ctx)
	if _, err := st.ApplyTombstones(ctx, Tombstones{}); err != nil {
		t.Fatal(err)
	}
	if added, _ := st.RestoreMembership(ctx, "remote-gone", b, ""); !added {
		t.Fatal("revived elsewhere: memberships must flow again")
	}
}
