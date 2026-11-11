package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/source"
	"github.com/EliasOlie/agyh/internal/store"
)

func TestIncrementalSync(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(text string) string {
		return `{"type":"user","cwd":"/proj","timestamp":"2026-11-01T10:00:00Z","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	f1 := filepath.Join(dir, "s1.jsonl")
	f2 := filepath.Join(dir, "s2.jsonl")
	os.WriteFile(f1, []byte(line("first session")), 0o644)
	os.WriteFile(f2, []byte(line("second session")), 0o644)

	st, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srcs := []source.Source{source.NewClaude(root), source.NewCodex(filepath.Join(root, "missing"))}

	stats, err := Sync(ctx, st, srcs, false)
	if err != nil || stats[0].Updated != 2 || stats[1].Available {
		t.Fatalf("first sync: %+v %v", stats, err)
	}
	stats, _ = Sync(ctx, st, srcs, false)
	if stats[0].Updated != 0 {
		t.Fatalf("unchanged files re-parsed: %+v", stats)
	}

	// Touch one file with new content, delete the other.
	os.WriteFile(f1, []byte(line("first session")+line("brand new message")), 0o644)
	future := time.Now().Add(time.Minute)
	os.Chtimes(f1, future, future)
	os.Remove(f2)
	stats, _ = Sync(ctx, st, srcs, false)
	if stats[0].Updated != 1 || stats[0].Removed != 1 {
		t.Fatalf("incremental sync: %+v", stats)
	}
	if hits, _ := st.Search(ctx, "brand new", store.Filter{}); len(hits) != 1 {
		t.Fatal("updated content not searchable")
	}
	stats, _ = Sync(ctx, st, srcs, true)
	if stats[0].Updated != 1 {
		t.Fatalf("full sync should re-parse everything: %+v", stats)
	}
}
