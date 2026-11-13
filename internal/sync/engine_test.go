package sync

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/source"
	"github.com/EliasOlie/agyh/internal/store"
)

func setupMockS3() (*httptest.Server, map[string][]byte) {
	storage := make(map[string][]byte)
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		key := strings.TrimPrefix(r.URL.Path, "/test-vault/")
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			storage[key] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, ok := storage[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		case http.MethodHead:
			_, ok := storage[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	return ts, storage
}

func TestEngineE2ESync(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	mockS3, _ := setupMockS3()
	defer mockS3.Close()

	cfg := config.Sync{
		Enabled:    true,
		Endpoint:   mockS3.URL,
		Bucket:     "test-vault",
		Region:     "us-east-1",
		AccessKey:  "mock-ak",
		SecretKey:  "mock-sk",
		Passphrase: "e2e-secret-passphrase",
		Prefix:     "vault",
	}

	ctx := context.Background()

	// 1. Setup Machine A with Store A
	dbPathA := filepath.Join(tmpDir, "dbA", "index.db")
	stA, err := store.Open(dbPathA)
	if err != nil {
		t.Fatalf("open store A: %v", err)
	}
	defer stA.Close()

	now := time.Now().Truncate(time.Second)
	sess1 := model.Session{
		Agent:     "agy",
		ID:        "session-1",
		Title:     "Credential rotation in Postgres",
		Workspace: "/work/pg",
		StartedAt: now.Add(-1 * time.Hour),
		UpdatedAt: now,
		Messages:  2,
	}
	msgs1 := []model.Message{
		{Role: "user", Text: "Please implement zero-downtime rotation", At: now.Add(-50 * time.Minute)},
		{Role: "assistant", Text: "Implemented dual-role rotation successfully", At: now},
	}
	if err := stA.Upsert(ctx, sess1, msgs1); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if err := stA.AddToProject(ctx, "security", sess1, "important credential rotation"); err != nil {
		t.Fatalf("add to project A: %v", err)
	}

	// Save archive locally for sess1 so source.LoadSessionOrArchive finds it
	if _, err := source.SaveArchive(sess1, msgs1); err != nil {
		t.Fatalf("save archive A: %v", err)
	}

	engineA, err := NewEngine(cfg, stA)
	if err != nil {
		t.Fatalf("new engine A: %v", err)
	}

	// 2. Machine A syncs (uploads to mock S3)
	resA, err := engineA.Sync(ctx)
	if err != nil {
		t.Fatalf("sync A failed: %v", err)
	}
	if resA.Uploaded != 1 {
		t.Fatalf("expected 1 uploaded, got %d", resA.Uploaded)
	}

	// 3. Setup Machine B with Store B (simulating a fresh machine)
	dbPathB := filepath.Join(tmpDir, "dbB", "index.db")
	stB, err := store.Open(dbPathB)
	if err != nil {
		t.Fatalf("open store B: %v", err)
	}
	defer stB.Close()

	engineB, err := NewEngine(cfg, stB)
	if err != nil {
		t.Fatalf("new engine B: %v", err)
	}

	// Check status before sync on Machine B
	statusB, err := engineB.Status(ctx)
	if err != nil {
		t.Fatalf("status B failed: %v", err)
	}
	if statusB.LocalSessions != 0 || statusB.RemoteSessions != 1 || statusB.ToDownload != 1 {
		t.Fatalf("unexpected status B: %+v", statusB)
	}

	// 4. Machine B syncs (downloads from mock S3)
	resB, err := engineB.Sync(ctx)
	if err != nil {
		t.Fatalf("sync B failed: %v", err)
	}
	if resB.Downloaded != 1 {
		t.Fatalf("expected 1 downloaded, got %d", resB.Downloaded)
	}

	// 5. Verify Machine B has the session in SQLite and FTS5
	hits, err := stB.Search(ctx, "rotation", store.Filter{})
	if err != nil {
		t.Fatalf("search B failed: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 search hit in B, got %d", len(hits))
	}
	if hits[0].ID != "session-1" {
		t.Fatalf("expected session-1, got %s", hits[0].ID)
	}
	if len(hits[0].Projects) != 1 || hits[0].Projects[0] != "security" {
		t.Fatalf("expected project 'security' restored, got %v", hits[0].Projects)
	}

	// 6. Verify Machine B can read full messages from archive
	sB, msgsB, err := source.LoadSessionOrArchive(ctx, hits[0].Session)
	if err != nil {
		t.Fatalf("LoadSessionOrArchive B failed: %v", err)
	}
	if len(msgsB) != 2 || msgsB[0].Text != "Please implement zero-downtime rotation" {
		t.Fatalf("messages in B mismatch: %+v", msgsB)
	}
	if sB.Title != "Credential rotation in Postgres" {
		t.Fatalf("session in B mismatch: %+v", sB)
	}
}
