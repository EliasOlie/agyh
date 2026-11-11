package bridge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/policy"
	"github.com/EliasOlie/agyh/internal/store"
)

// fakeChannel records what the bridge sends and can answer approvals.
type fakeChannel struct {
	name     string
	mu       sync.Mutex
	sent     []string
	prompted chan *Approval
	resolved []Decision
}

func newFake(name string) *fakeChannel {
	return &fakeChannel{name: name, prompted: make(chan *Approval, 10)}
}

func (f *fakeChannel) Name() string                             { return f.name }
func (f *fakeChannel) Markup() Markup                           { return waMarkup{} }
func (f *fakeChannel) Run(ctx context.Context, _ *Bridge) error { <-ctx.Done(); return ctx.Err() }
func (f *fakeChannel) Send(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeChannel) PromptApproval(_ context.Context, a *Approval) error {
	f.prompted <- a
	return nil
}
func (f *fakeChannel) ApprovalResolved(_ context.Context, _ *Approval, d Decision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, d)
	return nil
}
func (f *fakeChannel) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeChannel) resolvedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resolved)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type fakeRunner struct {
	mu   sync.Mutex
	dir  string
	argv []string
	done chan struct{}
}

func (r *fakeRunner) Run(_ context.Context, dir string, argv []string) (string, error) {
	r.mu.Lock()
	r.dir, r.argv = dir, argv
	r.mu.Unlock()
	defer close(r.done)
	return "all good", nil
}

func newBridge(t *testing.T, timeout int) (*Bridge, *store.Store, *httptest.Server, *config.Config) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.Bridge.Secret = "test-secret"
	cfg.ApprovalTimeoutSec = timeout
	cfg.Workspaces = []string{"/w/api", "/w/pdv"}
	b := New(Options{Config: cfg, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		StatePath: filepath.Join(t.TempDir(), "state.json")})
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)
	cfg.Bridge.Addr = strings.TrimPrefix(srv.URL, "http://")
	return b, st, srv, cfg
}

func shellReq(cmd string) hook.Request {
	return hook.Request{Agent: "claude", SessionID: "sess-1", Tool: "Bash", Command: cmd, Workspace: "/w/api"}
}

func TestAuthRequired(t *testing.T) {
	_, _, srv, _ := newBridge(t, 5)
	resp, err := http.Post(srv.URL+"/v1/approvals", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestPhoneApprovalAndAutonomousMode(t *testing.T) {
	b, _, _, cfg := newBridge(t, 5)
	tg, wa := newFake("telegram"), newFake("whatsapp")
	b.AddChannel(tg)
	b.AddChannel(wa)
	client := NewClient(cfg)
	ctx := context.Background()

	// Telegram answers first; WhatsApp's later answer is ignored.
	go func() {
		a := <-tg.prompted
		<-wa.prompted
		b.Decide(ctx, a.ID, Decision{Verdict: policy.Allow, By: "telegram"}, true)
		b.Decide(ctx, a.ID, Decision{Verdict: policy.Deny, By: "whatsapp"}, false)
	}()
	resp, err := client.Ask(ctx, "abc123", shellReq("npm install"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Verdict != "allow" || resp.By != "telegram" || resp.ID != "abc123" {
		t.Fatalf("unexpected: %+v", resp)
	}
	waitFor(t, func() bool { return tg.resolvedCount() == 1 && wa.resolvedCount() == 1 })

	// The session is now autonomous: no prompt, instant allow…
	resp, _ = client.Ask(ctx, "", shellReq("npm run build"))
	if resp.Verdict != "allow" || resp.By != "auto" {
		t.Fatalf("autonomous mode not applied: %+v", resp)
	}
	// …but fatal commands are still denied by the bridge itself.
	resp, _ = client.Ask(ctx, "", shellReq("git push --force"))
	if resp.Verdict != "deny" || resp.By != "policy" {
		t.Fatalf("fatal command allowed in autonomous mode: %+v", resp)
	}
	// Persisted across restarts.
	if !NewApprovals(b.approvals.statePath).IsAuto("claude:sess-1") {
		t.Fatal("autonomous mode not persisted")
	}
}

func TestTimeoutFallsBackToAsk(t *testing.T) {
	b, _, _, cfg := newBridge(t, 1)
	b.AddChannel(newFake("telegram"))
	start := time.Now()
	resp, err := NewClient(cfg).Ask(context.Background(), "", shellReq("make deploy"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Verdict != "ask" || resp.By != "timeout" || time.Since(start) > 3*time.Second {
		t.Fatalf("unexpected: %+v after %s", resp, time.Since(start))
	}
}

func TestTerminalDecisionUpdatesPhones(t *testing.T) {
	b, _, _, cfg := newBridge(t, 5)
	tg := newFake("telegram")
	b.AddChannel(tg)
	client := NewClient(cfg)
	ctx := context.Background()
	done := make(chan ApprovalResponse, 1)
	go func() {
		r, _ := client.Ask(ctx, "fedcba", shellReq("make deploy"))
		done <- r
	}()
	<-tg.prompted
	if err := client.Decide(ctx, "fedcba", "deny", "terminal", false); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.Verdict != "deny" || r.By != "terminal" {
		t.Fatalf("unexpected: %+v", r)
	}
	waitFor(t, func() bool { return tg.resolvedCount() == 1 })
}

func TestNoChannelMeansAsk(t *testing.T) {
	_, _, _, cfg := newBridge(t, 5)
	resp, _ := NewClient(cfg).Ask(context.Background(), "", shellReq("make"))
	if resp.Verdict != "ask" {
		t.Fatalf("got %+v", resp)
	}
}

func TestTextApprovals(t *testing.T) {
	b, _, _, cfg := newBridge(t, 5)
	wa := newFake("whatsapp")
	b.AddChannel(wa)
	ctx := context.Background()
	done := make(chan ApprovalResponse, 1)
	go func() {
		r, _ := NewClient(cfg).Ask(ctx, "", shellReq("make"))
		done <- r
	}()
	<-wa.prompted
	b.HandleText(ctx, wa, "y") // single pending approval: id optional
	if r := <-done; r.Verdict != "allow" || r.By != "whatsapp" {
		t.Fatalf("text approval failed: %+v", r)
	}
	b.HandleText(ctx, wa, "n deadbe")
	if !strings.Contains(wa.last(), "not found") {
		t.Fatalf("unknown id should be reported: %q", wa.last())
	}
}

func TestCommands(t *testing.T) {
	b, st, _, _ := newBridge(t, 5)
	ctx := context.Background()
	now := time.Now()
	err := st.Upsert(ctx, model.Session{Agent: "claude", ID: "aaaa1111bbbb", Title: "Fila idempotente",
		Workspace: "/w/api", UpdatedAt: now, StartedAt: now, Path: "/p", ModTime: now},
		[]model.Message{{Role: "user", Text: "webhook do whatsapp com retry"}})
	if err != nil {
		t.Fatal(err)
	}
	m := waMarkup{}
	if out := b.Execute(ctx, m, "test", "/chats"); !strings.Contains(out, "Fila idempotente") {
		t.Fatalf("/chats: %s", out)
	}
	if out := b.Execute(ctx, m, "test", "/search whatsapp"); !strings.Contains(out, "aaaa1111") {
		t.Fatalf("/search: %s", out)
	}
	if out := b.Execute(ctx, m, "test", "/save aaaa1111 pagamentos fila base"); !strings.Contains(out, "Saved") {
		t.Fatalf("/save: %s", out)
	}
	if out := b.Execute(ctx, m, "test", "/projects"); !strings.Contains(out, "pagamentos") {
		t.Fatalf("/projects: %s", out)
	}
	if out := b.Execute(ctx, m, "test", "/project pagamentos"); !strings.Contains(out, "Fila idempotente") {
		t.Fatalf("/project: %s", out)
	}
	if out := b.Execute(ctx, m, "test", "/show aaaa"); !strings.Contains(out, "agyh resume aaaa1111") {
		t.Fatalf("/show: %s", out)
	}
	if out := b.Execute(ctx, m, "test", "/help@agyh_bot"); !strings.Contains(out, "/search") {
		t.Fatalf("/help: %s", out)
	}
	if out := b.Execute(ctx, m, "test", "/nope"); !strings.Contains(out, "Unknown command") {
		t.Fatalf("unknown: %s", out)
	}
}

func TestNewSessionOnlyInConfiguredWorkspaces(t *testing.T) {
	b, _, _, _ := newBridge(t, 5)
	ch := newFake("telegram")
	b.AddChannel(ch)
	r := &fakeRunner{done: make(chan struct{})}
	b.runner = r
	m := waMarkup{}
	if out := b.Execute(context.Background(), m, "telegram", "/new @etc rm everything"); !strings.Contains(out, "unknown workspace") {
		t.Fatalf("unconfigured workspace accepted: %s", out)
	}
	out := b.Execute(context.Background(), m, "telegram", "/new claude @pdv add pix refunds; rm -rf /")
	if !strings.Contains(out, "Started") {
		t.Fatalf("/new: %s", out)
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir != "/w/pdv" || len(r.argv) != 3 || r.argv[2] != "add pix refunds; rm -rf /" {
		t.Fatalf("prompt must be one argv element, got dir=%s argv=%q", r.dir, r.argv)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(ch.last(), "finished") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(ch.last(), "all good") {
		t.Fatalf("result not broadcast: %q", ch.last())
	}
}

func TestNonLoopbackAddressRejected(t *testing.T) {
	if err := checkLoopback("0.0.0.0:9123"); err == nil {
		t.Fatal("0.0.0.0 must be rejected")
	}
	for _, ok := range []string{"127.0.0.1:9123", "localhost:1", "[::1]:9"} {
		if err := checkLoopback(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
}

func TestChunk(t *testing.T) {
	s := strings.Repeat("linha com acentuação\n", 400)
	parts := chunk(s, 1000)
	if len(parts) < 8 {
		t.Fatalf("expected several chunks, got %d", len(parts))
	}
	for _, p := range parts {
		if len(p) > 1000 || !json.Valid([]byte(`"`+strings.ReplaceAll(p, "\n", `\n`)+`"`)) {
			t.Fatalf("bad chunk (len %d)", len(p))
		}
	}
}
