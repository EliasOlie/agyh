package sync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/store"
)

// vault is an in-memory S3 that can be slowed down (to observe concurrency)
// and told to fail PUTs for some keys.
type vault struct {
	mu      gosync.Mutex
	objects map[string][]byte
	failPut func(key string) bool
	delay   time.Duration

	inflight, maxInflight atomic.Int32
	puts                  atomic.Int32 // session blobs
	allPuts               atomic.Int32 // every successful PUT
}

func (v *vault) get(key string) []byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.objects[key]
}

func (v *vault) set(key string, data []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.objects[key] = data
}

func newVault(t *testing.T) (*vault, config.Sync) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // archives of downloaded sessions
	v := &vault{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(srv.Close)
	return v, config.Sync{Enabled: true, Endpoint: srv.URL, Bucket: "test-vault", Region: "us-east-1",
		AccessKey: "ak", SecretKey: "sk", Passphrase: "test passphrase", Prefix: "vault"}
}

func (v *vault) serve(w http.ResponseWriter, r *http.Request) {
	n := v.inflight.Add(1)
	defer v.inflight.Add(-1)
	for {
		m := v.maxInflight.Load()
		if n <= m || v.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(v.delay)

	key := strings.TrimPrefix(r.URL.Path, "/test-vault/")
	v.mu.Lock()
	defer v.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		if v.failPut != nil && v.failPut(key) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if _, exists := v.objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		v.objects[key] = body
		v.allPuts.Add(1)
		if strings.Contains(key, "/sessions/") {
			v.puts.Add(1)
		}
	case http.MethodGet:
		data, ok := v.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type machine struct {
	st *store.Store
	e  *Engine
}

func newMachine(t *testing.T, cfg config.Sync) machine {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e, err := NewEngine(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	return machine{st: st, e: e}
}

var base = time.Now().Truncate(time.Second).Add(-time.Hour)

func testSession(i int) model.Session {
	return model.Session{Agent: "claude", ID: fmt.Sprintf("sess-%03d", i), Title: fmt.Sprintf("Session %d", i),
		StartedAt: base, UpdatedAt: base.Add(time.Duration(i) * time.Second), Messages: 1, Path: "synced:"}
}

func (m machine) add(t *testing.T, sessions ...model.Session) {
	t.Helper()
	for _, s := range sessions {
		if err := m.st.Upsert(context.Background(), s, []model.Message{{Role: "user", Text: s.Title}}); err != nil {
			t.Fatal(err)
		}
	}
}

func (m machine) sync(t *testing.T) SyncResult {
	t.Helper()
	res, err := m.e.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (m machine) status(t *testing.T) StatusReport {
	t.Helper()
	r, err := m.e.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (m machine) projects(t *testing.T, s model.Session) []string {
	t.Helper()
	hits, err := m.st.List(context.Background(), store.Filter{Agent: s.Agent, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == s.ID {
			return h.Projects
		}
	}
	t.Fatalf("session %s not found", s.ID)
	return nil
}

func assertInSync(t *testing.T, ms ...machine) {
	t.Helper()
	for i, m := range ms {
		if s := m.status(t); s.ToUpload != 0 || s.ToDownload != 0 {
			t.Fatalf("machine %d not in sync: %+v", i, s)
		}
	}
}

func TestProjectChangeAloneReachesOtherMachines(t *testing.T) {
	_, cfg := newVault(t)
	a, b := newMachine(t, cfg), newMachine(t, cfg)
	s := testSession(1)
	a.add(t, s)
	a.sync(t)
	b.sync(t)
	assertInSync(t, a, b)

	// Only the project membership changes: the session's UpdatedAt does not.
	if err := a.st.AddToProject(context.Background(), "pagamentos", s, "pix"); err != nil {
		t.Fatal(err)
	}
	if st := a.status(t); st.ToUpload != 1 {
		t.Fatalf("project change not detected: %+v", st)
	}
	if res := a.sync(t); res.Uploaded != 1 {
		t.Fatalf("project change not uploaded: %+v", res)
	}
	if st := b.status(t); st.ToDownload != 1 {
		t.Fatalf("other machine does not see the change: %+v", st)
	}
	b.sync(t)
	if got := b.projects(t, s); !slices.Equal(got, []string{"pagamentos"}) {
		t.Fatalf("project not merged on B: %v", got)
	}
	if notes, _ := b.st.Notes(context.Background(), "pagamentos"); notes[s.Key()] != "pix" {
		t.Fatalf("note not merged: %v", notes)
	}
	assertInSync(t, a, b)
}

func TestConcurrentProjectEditsConverge(t *testing.T) {
	v, cfg := newVault(t)
	a, b := newMachine(t, cfg), newMachine(t, cfg)
	s := testSession(1)
	a.add(t, s)
	a.sync(t)
	b.sync(t)

	ctx := context.Background()
	a.st.AddToProject(ctx, "alfa", s, "")
	b.st.AddToProject(ctx, "beta", s, "")
	a.sync(t)
	b.sync(t) // merges alfa, uploads the union
	a.sync(t) // merges beta

	for i, m := range []machine{a, b} {
		if got := m.projects(t, s); !slices.Equal(got, []string{"alfa", "beta"}) {
			t.Fatalf("machine %d: %v", i, got)
		}
	}
	assertInSync(t, a, b)

	// Converged: further syncs transfer nothing.
	puts := v.puts.Load()
	if res := a.sync(t); res.Uploaded != 0 || res.Downloaded != 0 {
		t.Fatalf("idle sync did work: %+v", res)
	}
	b.sync(t)
	if v.puts.Load() != puts {
		t.Fatal("idle syncs uploaded sessions")
	}
}

func TestManifestsWithoutHashStayCompatible(t *testing.T) {
	_, cfg := newVault(t)
	a := newMachine(t, cfg)
	plain, saved := testSession(1), testSession(2)
	a.add(t, plain, saved)
	ctx := context.Background()

	// A manifest written by an older agyh: no hashes at all.
	m := NewManifest()
	m.Sessions[plain.Key()] = summaryOf(plain, "")
	m.Sessions[saved.Key()] = summaryOf(saved, "")
	if err := a.e.saveManifest(ctx, m); err != nil {
		t.Fatal(err)
	}
	a.st.AddToProject(ctx, "pagamentos", saved, "")

	d, _, err := a.e.Diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Sessions without projects match an empty hash; only the one with projects is re-sent.
	if len(d.ToUpload) != 1 || d.ToUpload[0].ID != saved.ID || len(d.ToDownload) != 0 || len(d.ToMerge) != 0 {
		t.Fatalf("diff against an old manifest: %+v", d)
	}
}

func TestTransfersRunConcurrentlyWithinBound(t *testing.T) {
	v, cfg := newVault(t)
	v.delay = 15 * time.Millisecond
	a, b := newMachine(t, cfg), newMachine(t, cfg)
	const n = 30
	for i := range n {
		a.add(t, testSession(i))
	}
	if res := a.sync(t); res.Uploaded != n || res.Total != n {
		t.Fatalf("upload: %+v", res)
	}
	if m := v.maxInflight.Load(); m < 2 || m > syncWorkers {
		t.Fatalf("uploads: max %d concurrent requests, want 2..%d", m, syncWorkers)
	}
	v.maxInflight.Store(0)
	if res, err := b.e.Restore(context.Background()); err != nil || res.Downloaded != n {
		t.Fatalf("restore: %+v %v", res, err)
	}
	if m := v.maxInflight.Load(); m < 2 || m > syncWorkers {
		t.Fatalf("restore: max %d concurrent requests", m)
	}
	if c, _ := b.st.Count(context.Background()); c["claude"] != n {
		t.Fatalf("restored %v", c)
	}
}

func TestPartialUploadFailureKeepsSuccessfulOnes(t *testing.T) {
	v, cfg := newVault(t)
	a := newMachine(t, cfg)
	for i := range 10 {
		a.add(t, testSession(i))
	}
	bad := testSession(7)
	v.failPut = func(key string) bool { return strings.HasSuffix(key, bad.ID+".blob") }

	_, err := a.e.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), bad.Key()) {
		t.Fatalf("expected the failing session in the error, got %v", err)
	}
	m, err := a.e.fetchManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Sessions[bad.Key()]; ok {
		t.Fatal("failed upload recorded in the manifest")
	}
	// One failure must not cancel the others: all 9 good sessions are recorded.
	if len(m.Sessions) != 9 {
		t.Fatalf("manifest has %d sessions, want the 9 that succeeded", len(m.Sessions))
	}

	// Once S3 recovers, only what is missing is uploaded.
	v.failPut = nil
	if st := a.status(t); st.ToUpload != 1 {
		t.Fatalf("after recovery: %+v", st)
	}
	if res := a.sync(t); res.Uploaded != 1 || res.Total != 10 {
		t.Fatalf("recovery sync: %+v", res)
	}
	assertInSync(t, a)
}

func TestMetaHash(t *testing.T) {
	if metaHash(nil, nil) != "" || metaHash([]string{}, map[string]string{"x": "y"}) != "" {
		t.Fatal("no projects must hash to empty")
	}
	h := metaHash([]string{"b", "a", "a"}, map[string]string{"a": "n"})
	if h == "" || h != metaHash([]string{"a", "b"}, map[string]string{"a": "n"}) {
		t.Fatal("hash must ignore order and duplicates")
	}
	if h == metaHash([]string{"a", "b"}, map[string]string{"a": "other"}) {
		t.Fatal("note change must change the hash")
	}
	if metaHash([]string{"a=b"}, nil) == metaHash([]string{"a"}, map[string]string{"a": "b"}) {
		t.Fatal("hash must not be ambiguous")
	}
}
