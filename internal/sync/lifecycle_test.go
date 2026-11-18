package sync

import (
	"context"
	"slices"
	"testing"
	"time"
)

func projectNames(t *testing.T, m machine) []string {
	t.Helper()
	ps, err := m.st.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	slices.Sort(out)
	return out
}

func TestProjectDeletionsTravelThroughTheVault(t *testing.T) {
	_, cfg := newVault(t)
	off := false
	cfg.Kit = &off
	ctx := context.Background()
	a, b := newMachine(t, cfg), newMachine(t, cfg)
	s1, s2 := testSession(1), testSession(2)
	a.add(t, s1, s2)
	a.st.AddToProject(ctx, "conecta-me", s1, "")
	a.st.AddToProject(ctx, "conectame", s2, "dup")
	a.sync(t)
	b.sync(t)
	if got := projectNames(t, b); !slices.Equal(got, []string{"conecta-me", "conectame"}) {
		t.Fatalf("b before: %v", got)
	}

	// Merge the duplicate on A: B follows, sessions stay, nothing comes back.
	if _, err := a.st.MergeProjects(ctx, "conectame", "conecta-me"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		a.sync(t)
		b.sync(t)
	}
	for name, m := range map[string]machine{"a": a, "b": b} {
		if got := projectNames(t, m); !slices.Equal(got, []string{"conecta-me"}) {
			t.Fatalf("%s after merge: %v", name, got)
		}
		if got := m.projects(t, s2); !slices.Equal(got, []string{"conecta-me"}) {
			t.Fatalf("%s: merged session should be in conecta-me: %v", name, got)
		}
		if c, _ := m.st.Count(ctx); c["claude"] != 2 {
			t.Fatalf("%s: sessions must survive: %v", name, c)
		}
	}

	// Removing a session from a project on B propagates to A.
	b.st.RemoveFromProject(ctx, "conecta-me", s1)
	for range 2 {
		b.sync(t)
		a.sync(t)
	}
	if got := a.projects(t, s1); len(got) != 0 {
		t.Fatalf("membership removal did not propagate: %v", got)
	}

	// Recreating a deleted name later wins over the old deletion.
	time.Sleep(1100 * time.Millisecond) // timestamps have one-second resolution
	b.st.AddToProject(ctx, "conectame", s1, "again")
	for range 2 {
		b.sync(t)
		a.sync(t)
	}
	if got := a.projects(t, s1); !slices.Equal(got, []string{"conectame"}) {
		t.Fatalf("a recreated project must reach the other machine: %v", got)
	}
	assertInSync(t, a, b)
}
