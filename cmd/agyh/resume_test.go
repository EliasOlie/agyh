package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/store"
	vaultsync "github.com/EliasOlie/agyh/internal/sync"
)

// archived adds a session that came from another machine (text only).
func archived(t *testing.T, s model.Session) {
	t.Helper()
	st, err := store.Open(os.Getenv("AGYH_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s.Path = "archive:/nowhere/" + s.ID + ".json.gz"
	if err := st.Upsert(context.Background(), s, []model.Message{{Role: "user", Text: s.Title}}); err != nil {
		t.Fatal(err)
	}
}

// A session from the old machine reopens here: its file comes back from the
// vault into the folder Claude Code reads for that workspace.
func TestResumeRestoresTheTranscriptFromTheVault(t *testing.T) {
	home, _ := env(t)
	fakeAgents(t, "claude", "agy")
	ws := filepath.Join(home, "work", "_app")
	os.MkdirAll(ws, 0o755)
	id := "cccc5555-0000-4000-8000-000000000005"
	archived(t, model.Session{Agent: "claude", ID: id, Title: "Checkout", Workspace: ws,
		StartedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now()})

	fetches := 0
	oldFetch, oldExec := fetchTranscript, execFn
	t.Cleanup(func() { fetchTranscript, execFn = oldFetch, oldExec })
	fetchTranscript = func(_ context.Context, _ config.Sync, agent, sid string) (string, []byte, error) {
		fetches++
		if agent != "claude" || sid != id {
			t.Fatalf("fetched %s:%s", agent, sid)
		}
		return id + ".jsonl", []byte(`{"type":"user"}` + "\n"), nil
	}
	var argv []string
	execFn = func(_ string, a []string) error { argv = a; return nil }

	mustRun(t, "resume", id[:8])
	want := filepath.Join(home, "claude", strings.ReplaceAll(strings.ReplaceAll(ws, "/", "-"), "_", "-"), id+".jsonl")
	if b, err := os.ReadFile(want); err != nil || string(b) != `{"type":"user"}`+"\n" {
		t.Fatalf("transcript not at %s: %v", want, err)
	}
	if strings.Join(argv, " ") != "claude --resume "+id {
		t.Fatalf("argv: %q", argv)
	}
	// Already placed: no second download.
	mustRun(t, "resume", id[:8])
	if fetches != 1 {
		t.Fatalf("downloaded %d times", fetches)
	}
}

func TestResumeExplainsWhatCannotBeReopened(t *testing.T) {
	env(t)
	fakeAgents(t, "claude", "agy")
	archived(t, model.Session{Agent: "agy", ID: "dddd6666-0000-4000-8000-000000000006", Title: "Old agy chat",
		StartedAt: time.Now(), UpdatedAt: time.Now()})
	archived(t, model.Session{Agent: "claude", ID: "eeee7777-0000-4000-8000-000000000007", Title: "Text only",
		StartedAt: time.Now(), UpdatedAt: time.Now()})
	old := fetchTranscript
	t.Cleanup(func() { fetchTranscript = old })
	fetchTranscript = func(context.Context, config.Sync, string, string) (string, []byte, error) {
		return "", nil, vaultsync.ErrNoTranscript
	}
	if _, err := run(t, "", "resume", "dddd6666"); err == nil || !strings.Contains(err.Error(), "agyh show dddd6666") {
		t.Fatalf("agy: %v", err)
	}
	if _, err := run(t, "", "resume", "eeee7777"); err == nil || !strings.Contains(err.Error(), "only has its text") {
		t.Fatalf("text only: %v", err)
	}
}
