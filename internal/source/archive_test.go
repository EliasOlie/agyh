package source

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/model"
)

func TestArchiveSaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	now := time.Now().Truncate(time.Second)
	sess := model.Session{
		Agent:     "agy",
		ID:        "test-session-1234",
		Title:     "Testing Archive",
		Workspace: "/home/user/work",
		StartedAt: now.Add(-10 * time.Minute),
		UpdatedAt: now,
		Messages:  2,
	}
	msgs := []model.Message{
		{Role: "user", Text: "Hello world", At: now.Add(-5 * time.Minute)},
		{Role: "assistant", Text: "Hello! How can I help?", At: now},
	}

	path, err := SaveArchive(sess, msgs)
	if err != nil {
		t.Fatalf("SaveArchive failed: %v", err)
	}
	expected := filepath.Join(tmpDir, "agyh", "archive", "agy", "test-session-1234.json.gz")
	if path != expected {
		t.Errorf("expected path %s, got %s", expected, path)
	}

	// Load directly
	loadedSess, loadedMsgs, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("LoadArchive failed: %v", err)
	}
	if loadedSess.ID != sess.ID || loadedSess.Title != sess.Title {
		t.Errorf("session mismatch: %+v vs %+v", loadedSess, sess)
	}
	if len(loadedMsgs) != 2 || loadedMsgs[0].Text != "Hello world" {
		t.Errorf("messages mismatch: %+v", loadedMsgs)
	}

	// Load via LoadSessionOrArchive with archive: prefix
	sess.Path = "archive:" + path
	s2, m2, err := LoadSessionOrArchive(context.Background(), sess)
	if err != nil {
		t.Fatalf("LoadSessionOrArchive failed: %v", err)
	}
	if s2.ID != sess.ID || len(m2) != 2 {
		t.Errorf("LoadSessionOrArchive mismatch")
	}

	// Load via fallback when original path does not exist
	sess.Path = "/non/existent/file.jsonl"
	s3, m3, err := LoadSessionOrArchive(context.Background(), sess)
	if err != nil {
		t.Fatalf("LoadSessionOrArchive fallback failed: %v", err)
	}
	if s3.ID != sess.ID || len(m3) != 2 {
		t.Errorf("LoadSessionOrArchive fallback mismatch")
	}
}
