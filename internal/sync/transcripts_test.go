package sync

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
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

// A long session on a server that refuses large objects: the transcript
// goes in parts and comes back whole. If even a part is refused, the
// session and the kit are still synced, and the transcript waits for the
// next sync instead of failing every one.
func TestBigTranscriptsGoInParts(t *testing.T) {
	v, cfg := newVault(t)
	ctx := context.Background()
	oldPart := transcriptPart
	transcriptPart = 64 << 10
	t.Cleanup(func() { transcriptPart = oldPart })
	v.maxObject = 100 << 10

	// Incompressible content, so the gzipped file really is big.
	raw := noise(300 << 10)
	file := filepath.Join(t.TempDir(), "long.jsonl")
	os.WriteFile(file, raw, 0o600)
	s := testSession(1)
	s.Path = file

	a := newMachine(t, cfg)
	a.add(t, s)
	res := a.sync(t)
	if len(res.Warnings) != 0 || res.Kit == nil {
		t.Fatalf("warnings %v, kit %v", res.Warnings, res.Kit)
	}
	m, _ := a.e.fetchManifest(ctx)
	if p := m.Sessions[s.Key()].TranscriptParts; p < 4 {
		t.Fatalf("stored in %d parts", p)
	}
	name, data, err := newMachine(t, cfg).e.FetchTranscript(ctx, "claude", s.ID)
	if err != nil || name != "long.jsonl" || !bytes.Equal(data, raw) {
		t.Fatalf("fetch: %q %d bytes %v", name, len(data), err)
	}

	// A server that refuses even the parts: a warning, not a failed sync.
	v.maxObject = 1 << 10
	s2 := testSession(2)
	s2.Path = file
	a.add(t, s2)
	res, err = a.e.Sync(ctx)
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "retried next sync") || res.Kit == nil {
		t.Fatalf("sync: %v %v", err, res.Warnings)
	}
	if r := a.status(t); r.ToUpload != 0 || r.Transcripts != 1 || r.TranscriptsPossible != 2 {
		t.Fatalf("status: %+v", r)
	}
	// The server accepts it again: the next sync stores it.
	v.maxObject = 100 << 10
	if res := a.sync(t); len(res.Warnings) != 0 {
		t.Fatal(res.Warnings)
	}
	if r := a.status(t); r.Transcripts != 2 {
		t.Fatalf("not retried: %d of %d", r.Transcripts, r.TranscriptsPossible)
	}
}

// Parts left half old, half new by an interrupted upload are never handed out.
func TestIncompleteTranscriptIsRefused(t *testing.T) {
	v, cfg := newVault(t)
	ctx := context.Background()
	oldPart := transcriptPart
	transcriptPart = 1 << 10
	t.Cleanup(func() { transcriptPart = oldPart })
	raw := noise(20 << 10)
	file := filepath.Join(t.TempDir(), "x.jsonl")
	os.WriteFile(file, raw, 0o600)
	s := testSession(1)
	s.Path = file
	a := newMachine(t, cfg)
	a.add(t, s)
	a.sync(t)
	// Replace part 2 with another valid, encrypted part.
	v.set("vault/transcripts/claude/"+s.ID+".2.blob", v.get("vault/transcripts/claude/"+s.ID+".3.blob"))
	if _, _, err := a.e.FetchTranscript(ctx, "claude", s.ID); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("mixed parts: %v", err)
	}
}

// noise is incompressible, so a gzipped transcript stays big.
func noise(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return b
}
