package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/EliasOlie/agyh/internal/config"
)

// The agent's own session file travels through the vault, including for
// sessions backed up before agyh kept transcripts, and survives other
// machines re-uploading the session from its text.
func TestTranscriptsTravelThroughTheVault(t *testing.T) {
	_, cfg := newVault(t)
	off := false
	cfg.Kit = &off
	ctx := context.Background()

	raw := []byte(`{"type":"user","message":{"role":"user","content":"oi"},"cwd":"/w/app"}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}` + "\n")
	file := filepath.Join(t.TempDir(), "sess-001.jsonl")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := testSession(1)
	s.Path = file

	// An older agyh: the vault gets the text only.
	old := cfg
	old.Transcripts = &off
	a := newMachine(t, old)
	a.add(t, s)
	a.sync(t)
	if r := a.status(t); r.Transcripts != 0 || r.TranscriptsPossible != 1 {
		t.Fatalf("before: %d of %d", r.Transcripts, r.TranscriptsPossible)
	}

	// This agyh adds the file on its next sync, though the session did not change.
	a = machine{st: a.st, e: mustEngine(t, cfg, a)}
	a.sync(t)
	if r := a.status(t); r.Transcripts != 1 {
		t.Fatalf("not backfilled: %d of %d", r.Transcripts, r.TranscriptsPossible)
	}

	// Another machine gets the file back, byte for byte.
	b := newMachine(t, cfg)
	b.sync(t)
	name, data, err := b.e.FetchTranscript(ctx, "claude", s.ID)
	if err != nil || name != "sess-001.jsonl" || string(data) != string(raw) {
		t.Fatalf("fetch: %q %q %v", name, data, err)
	}
	// B only has the text: re-uploading everything must not drop A's file.
	if _, err := b.e.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if r := b.status(t); r.Transcripts != 1 {
		t.Fatal("a machine without the file erased the vault's transcript")
	}
	if _, _, err := b.e.FetchTranscript(ctx, "claude", "sess-999"); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("unknown session: %v", err)
	}
}

func mustEngine(t *testing.T, cfg config.Sync, m machine) *Engine {
	t.Helper()
	e, err := NewEngine(cfg, m.st)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
