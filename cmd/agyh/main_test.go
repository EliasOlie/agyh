package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/index"
	"github.com/EliasOlie/agyh/internal/policy"
	"github.com/EliasOlie/agyh/internal/store"
	agsync "github.com/EliasOlie/agyh/internal/sync"
)

// Two Claude sessions sharing the id prefix "aaaa", so prefix resolution can be
// exercised both ways. "pix" is the most recent one.
const (
	pixID  = "aaaa1111-0000-4000-8000-000000000001"
	authID = "aaaa2222-0000-4000-8000-000000000002"
)

// env isolates every path agyh reads or writes inside a temp HOME. It returns
// the HOME and the transcript path of the "pix" session.
func env(t *testing.T) (home, pixPath string) {
	t.Helper()
	home = t.TempDir()
	for k, v := range map[string]string{
		"HOME":                 home,
		"XDG_DATA_HOME":        filepath.Join(home, "data"),
		"XDG_CONFIG_HOME":      filepath.Join(home, "config"),
		"AGYH_CONFIG_DIR":      filepath.Join(home, "config", "agyh"),
		"AGYH_DB":              filepath.Join(home, "data", "index.db"),
		"AGYH_AGY_DIR":         filepath.Join(home, "no-agy"),
		"AGYH_CLAUDE_DIR":      filepath.Join(home, "claude"),
		"AGYH_CODEX_DIR":       filepath.Join(home, "no-codex"),
		"CLAUDE_CONFIG_DIR":    "",
		"CODEX_HOME":           "",
		"AGYH_TELEGRAM_TOKEN":  "",
		"AGYH_SYNC_ENDPOINT":   "",
		"AGYH_SYNC_BUCKET":     "",
		"AGYH_SYNC_ACCESS_KEY": "",
		"AGYH_SYNC_SECRET_KEY": "",
		"AGYH_SYNC_PASSPHRASE": "",
	} {
		t.Setenv(k, v)
	}
	now := time.Now().UTC()
	pixPath = writeSession(t, home, "-work-pix", pixID, "/work/pix", "Pix webhook", now.Add(-time.Hour),
		"Corrige o webhook do Pix", "Webhook do Pix corrigido com idempotência.")
	writeSession(t, home, "-work-auth", authID, "/work/auth", "Auth rotation", now.Add(-48*time.Hour),
		"Implementa rotação de credenciais", "Rotação feita sem downtime.")
	return home, pixPath
}

func writeSession(t *testing.T, home, dir, id, cwd, title string, at time.Time, user, assistant string) string {
	t.Helper()
	ts := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339) }
	lines := []string{
		fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`, id, cwd, ts(0), user),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, ts(time.Minute), assistant),
		fmt.Sprintf(`{"type":"custom-title","customTitle":%q,"sessionId":%q}`, title, id),
	}
	p := filepath.Join(home, "claude", dir, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// run executes the CLI in-process, like a user would from the shell.
func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := run(t, "", args...)
	if err != nil {
		t.Fatalf("agyh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func writeConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	b, _ := json.Marshal(cfg)
	dir := os.Getenv("AGYH_CONFIG_DIR")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// closedAddr is a loopback address nobody listens on: a bridge that is "not running".
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// fakeBridge answers the hook API like a running `agyh bridge` would.
type fakeBridge struct {
	srv     *httptest.Server
	verdict string

	mu       sync.Mutex
	auth     []string
	notified []string
	decided  []map[string]any
}

func newFakeBridge(t *testing.T, verdict string) *fakeBridge {
	t.Helper()
	fb := &fakeBridge{verdict: verdict}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) { fb.record(r) })
	mux.HandleFunc("POST /v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		fb.record(r)
		var in bridge.ApprovalRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(bridge.ApprovalResponse{ID: in.ID, Verdict: fb.verdict, Reason: "tapped", By: "telegram"})
	})
	mux.HandleFunc("POST /v1/approvals/{id}/decision", func(w http.ResponseWriter, r *http.Request) {
		fb.record(r)
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		fb.mu.Lock()
		fb.decided = append(fb.decided, in)
		fb.mu.Unlock()
	})
	mux.HandleFunc("POST /v1/notify", func(w http.ResponseWriter, r *http.Request) {
		fb.record(r)
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		fb.mu.Lock()
		fb.notified = append(fb.notified, in["text"])
		fb.mu.Unlock()
	})
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBridge) record(r *http.Request) {
	fb.mu.Lock()
	fb.auth = append(fb.auth, r.Header.Get("Authorization"))
	fb.mu.Unlock()
}

func (fb *fakeBridge) addr() string { return strings.TrimPrefix(fb.srv.URL, "http://") }

func (fb *fakeBridge) config(secret string) *config.Config {
	cfg := config.Default()
	cfg.Bridge = config.Bridge{Addr: fb.addr(), Secret: secret}
	cfg.ApprovalTimeoutSec = 5
	return cfg
}

// ---------------------------------------------------------------- browsing

func TestListAndSearch(t *testing.T) {
	env(t)

	var hits []store.Hit
	if err := json.Unmarshal([]byte(mustRun(t, "ls", "--json")), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != pixID || hits[1].ID != authID {
		t.Fatalf("ls --json: want pix then auth, got %+v", hits)
	}
	if hits[0].Title != "Pix webhook" || hits[0].Workspace != "/work/pix" {
		t.Fatalf("metadata not indexed: %+v", hits[0])
	}

	out := mustRun(t, "ls", "-n", "1")
	if !strings.Contains(out, "ID") || !strings.Contains(out, "aaaa1111") || strings.Contains(out, "aaaa2222") {
		t.Fatalf("ls -n 1 should show only the newest session:\n%s", out)
	}

	// Accent-insensitive, prefix match, through the CLI.
	if err := json.Unmarshal([]byte(mustRun(t, "search", "rotacao", "credenc", "--json")), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != authID || hits[0].Snippet == "" {
		t.Fatalf("search: %+v", hits)
	}

	if out := mustRun(t, "search", "inexistente"); !strings.Contains(out, "No sessions found.") {
		t.Fatalf("empty search: %q", out)
	}
	if out := strings.TrimSpace(mustRun(t, "search", "inexistente", "--json")); out != "[]" {
		t.Fatalf("empty --json must be [], got %q", out)
	}
	if _, err := run(t, "", "search"); err == nil {
		t.Fatal("search without words accepted")
	}
	if _, err := run(t, "", "ls", "extra"); err == nil {
		t.Fatal("ls accepted positional args")
	}

	// Filters reach the store.
	if err := json.Unmarshal([]byte(mustRun(t, "ls", "--workspace", "auth", "--json")), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != authID {
		t.Fatalf("--workspace filter: %+v", hits)
	}
	if err := json.Unmarshal([]byte(mustRun(t, "ls", "--agent", "codex", "--json")), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("--agent filter: %+v", hits)
	}
}

func TestShow(t *testing.T) {
	env(t)

	out := mustRun(t, "show", "aaaa1111")
	for _, want := range []string{"Pix webhook", "id: " + pixID, "resume: cd /work/pix && claude --resume " + pixID,
		"› you", "Corrige o webhook do Pix", "◆ claude", "idempotência"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show is missing %q:\n%s", want, out)
		}
	}

	out = mustRun(t, "show", "aaaa1111", "--last", "1")
	if strings.Contains(out, "Corrige o webhook") || !strings.Contains(out, "idempotência") {
		t.Fatalf("--last 1 should keep only the final message:\n%s", out)
	}

	var got struct {
		Session  map[string]any   `json:"session"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "show", "claude:"+authID, "--json")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Session["id"] != authID || len(got.Messages) != 2 {
		t.Fatalf("show --json: %+v", got)
	}

	_, err := run(t, "", "show", "aaaa")
	if err == nil || !strings.Contains(err.Error(), "matches several sessions") ||
		!strings.Contains(err.Error(), pixID) || !strings.Contains(err.Error(), authID) {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	_, err = run(t, "", "show", "zzzz")
	if err == nil || !strings.Contains(err.Error(), `no session matches "zzzz"`) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestShowOfDeletedSession(t *testing.T) {
	_, pixPath := env(t)
	mustRun(t, "index")
	if err := os.Remove(pixPath); err != nil {
		t.Fatal(err)
	}
	// The next sync drops the session (its file is gone), so show must fail cleanly, not panic.
	if _, err := run(t, "", "show", "aaaa1111"); err == nil {
		t.Fatal("show of a deleted session succeeded")
	}
}

func TestResumePrint(t *testing.T) {
	env(t)
	if out := mustRun(t, "resume", "aaaa2222", "--print"); out != "cd /work/auth && claude --resume "+authID+"\n" {
		t.Fatalf("resume --print: %q", out)
	}
	if out := mustRun(t, "resume", "--last", "--print"); !strings.Contains(out, pixID) {
		t.Fatalf("resume --last should pick the newest session: %q", out)
	}
	if _, err := run(t, "", "resume", "--last", "--agent", "codex", "--print"); err == nil {
		t.Fatal("resume --last for an agent without sessions succeeded")
	}
	if _, err := run(t, "", "resume"); err == nil || !strings.Contains(err.Error(), "--last") {
		t.Fatalf("resume without id: %v", err)
	}
}

func TestIndex(t *testing.T) {
	env(t)
	var stats []index.Stats
	if err := json.Unmarshal([]byte(mustRun(t, "index", "--json")), &stats); err != nil {
		t.Fatal(err)
	}
	by := map[string]index.Stats{}
	for _, s := range stats {
		by[s.Agent] = s
	}
	if c := by["claude"]; !c.Available || c.Seen != 2 || c.Updated != 2 {
		t.Fatalf("claude stats: %+v", c)
	}
	if by["agy"].Available || by["codex"].Available {
		t.Fatalf("missing agents reported as available: %+v", stats)
	}

	// Second run is incremental; --full re-reads everything.
	if err := json.Unmarshal([]byte(mustRun(t, "index", "--json")), &stats); err != nil {
		t.Fatal(err)
	}
	for _, s := range stats {
		if s.Agent == "claude" && s.Updated != 0 {
			t.Fatalf("unchanged sessions re-parsed: %+v", s)
		}
	}
	out := mustRun(t, "index", "--full")
	if !strings.Contains(out, "AGENT") || !strings.Contains(out, "claude") {
		t.Fatalf("index table: %s", out)
	}
}

// ---------------------------------------------------------------- projects

func TestProjectLifecycle(t *testing.T) {
	env(t)

	if out := mustRun(t, "project", "ls"); !strings.Contains(out, "No projects yet") {
		t.Fatalf("empty project ls: %q", out)
	}
	if out := strings.TrimSpace(mustRun(t, "project", "ls", "--json")); out != "[]" {
		t.Fatalf("empty project ls --json: %q", out)
	}

	mustRun(t, "project", "new", "pagamentos", "-d", "Pix e boletos")
	if _, err := run(t, "", "project", "new", "pagamentos"); err == nil {
		t.Fatal("duplicate project accepted")
	}

	// add creates the project when needed and accepts several ids.
	out := mustRun(t, "project", "add", "seguranca", "aaaa2222", "aaaa1111", "--note", "revisar")
	if strings.Count(out, "Saved claude:") != 2 {
		t.Fatalf("project add: %q", out)
	}
	if _, err := run(t, "", "project", "add", "seguranca", "aaaa"); err == nil {
		t.Fatal("ambiguous id added to a project")
	}

	var ps []store.Project
	if err := json.Unmarshal([]byte(mustRun(t, "project", "ls", "--json")), &ps); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, p := range ps {
		counts[p.Name] = p.Sessions
	}
	if counts["pagamentos"] != 0 || counts["seguranca"] != 2 {
		t.Fatalf("project counts: %+v", ps)
	}

	var hits []store.Hit
	if err := json.Unmarshal([]byte(mustRun(t, "project", "show", "seguranca", "--json")), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || len(hits[0].Projects) != 1 || hits[0].Projects[0] != "seguranca" {
		t.Fatalf("project show: %+v", hits)
	}
	// Browsing commands tag sessions with their projects.
	if out := mustRun(t, "ls"); !strings.Contains(out, "#seguranca") {
		t.Fatalf("ls should show project tags:\n%s", out)
	}
	if err := json.Unmarshal([]byte(mustRun(t, "ls", "-p", "seguranca", "--json")), &hits); err != nil || len(hits) != 2 {
		t.Fatalf("ls -p: %+v %v", hits, err)
	}

	mustRun(t, "project", "rm", "seguranca", "aaaa1111")
	if _, err := run(t, "", "project", "rm", "seguranca", "aaaa1111"); err == nil || !strings.Contains(err.Error(), "is not in project") {
		t.Fatalf("removing twice: %v", err)
	}

	mustRun(t, "project", "rename", "seguranca", "auth")
	if _, err := run(t, "", "project", "rename", "seguranca", "x"); err == nil || !strings.Contains(err.Error(), "no project named") {
		t.Fatalf("rename missing: %v", err)
	}
	if err := json.Unmarshal([]byte(mustRun(t, "project", "show", "auth", "--json")), &hits); err != nil || len(hits) != 1 || hits[0].ID != authID {
		t.Fatalf("membership must follow a rename: %+v %v", hits, err)
	}

	mustRun(t, "project", "delete", "auth")
	if _, err := run(t, "", "project", "delete", "auth"); err == nil || !strings.Contains(err.Error(), "no project named") {
		t.Fatalf("delete missing: %v", err)
	}
	// Deleting a project never touches the sessions themselves.
	if err := json.Unmarshal([]byte(mustRun(t, "ls", "--json")), &hits); err != nil || len(hits) != 2 {
		t.Fatalf("sessions after project delete: %+v %v", hits, err)
	}
}

// ---------------------------------------------------------------- hooks

func TestInstallHooksPrintsValidJSON(t *testing.T) {
	for agent, want := range map[string]string{
		"claude": "agyh hook pre-tool-use --agent claude",
		"agy":    "agyh hook pre-tool-use --agent agy",
	} {
		out := mustRun(t, "install-hooks", agent)
		comment, body, ok := strings.Cut(out, "\n")
		if !ok || !strings.HasPrefix(comment, "// Add to ") {
			t.Fatalf("%s: missing header: %q", agent, out)
		}
		if !json.Valid([]byte(body)) {
			t.Fatalf("%s: body is not JSON:\n%s", agent, body)
		}
		if !strings.Contains(body, want) || !strings.Contains(body, "agyh hook stop --agent "+agent) {
			t.Fatalf("%s: hook commands missing:\n%s", agent, body)
		}
	}
	if _, err := run(t, "", "install-hooks", "codex"); err == nil {
		t.Fatal("unsupported agent accepted")
	}
}

func TestUseTTY(t *testing.T) {
	for _, c := range []struct {
		agent, flag string
		want        bool
	}{
		{"agy", "auto", true},
		{"claude", "auto", false},
		{"claude", "on", true},
		{"claude", "yes", true},
		{"agy", "off", false},
		{"agy", "false", false},
	} {
		if got := useTTY(c.agent, c.flag); got != c.want {
			t.Errorf("useTTY(%q, %q) = %v, want %v", c.agent, c.flag, got, c.want)
		}
	}
}

func claudeHookInput(command string) string {
	b, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": "/w", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	return string(b)
}

func hookDecision(t *testing.T, out string) (decision, reason string) {
	t.Helper()
	var v struct {
		H struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("hook output is not Claude Code JSON: %q (%v)", out, err)
	}
	return v.H.Decision, v.H.Reason
}

func TestPreToolUseHookWithoutBridge(t *testing.T) {
	env(t)
	writeConfig(t, map[string]any{"bridge": map[string]any{"addr": closedAddr(t), "secret": "s3cr3t"},
		"approval_timeout_sec": 2})

	for _, c := range []struct {
		command, want string
	}{
		{"sudo rm -rf /", "deny"},      // fatal: denied locally, no network
		{"git status", "allow"},        // read-only: allowed locally
		{"ls | sort > out.txt", "ask"}, // redirection: needs a human
		{"npm install", "ask"},         // no bridge: fall back to Claude Code's prompt
	} {
		out, err := run(t, claudeHookInput(c.command), "hook", "pre-tool-use", "--agent", "claude", "--tty", "off")
		if err != nil {
			t.Fatalf("%q: hook must never fail: %v", c.command, err)
		}
		if d, reason := hookDecision(t, out); d != c.want {
			t.Errorf("%q: decision %s (%s), want %s", c.command, d, reason, c.want)
		}
	}

	// Garbage input must not break the agent: Claude gets "ask".
	out, err := run(t, "not json", "hook", "pre-tool-use")
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := hookDecision(t, out); d != "ask" {
		t.Fatalf("bad input: %s", d)
	}

	// Antigravity has no "ask": anything undecided is denied.
	agyIn := `{"conversationId":"c1","workspacePaths":["/w"],"toolCall":{"args":{"CommandLine":"npm install"}}}`
	out, err = run(t, agyIn, "hook", "pre-tool-use", "--agent", "agy", "--tty", "off")
	if err != nil {
		t.Fatal(err)
	}
	var agy struct{ Decision string }
	if json.Unmarshal([]byte(out), &agy) != nil || agy.Decision != "deny" {
		t.Fatalf("agy undecided: %q", out)
	}
}

func TestPreToolUseHookAsksTheBridge(t *testing.T) {
	env(t)
	fb := newFakeBridge(t, "allow")
	writeConfig(t, map[string]any{"bridge": map[string]any{"addr": fb.addr(), "secret": "s3cr3t"}})

	out, err := run(t, claudeHookInput("npm install"), "hook", "pre-tool-use", "--tty", "off")
	if err != nil {
		t.Fatal(err)
	}
	if d, reason := hookDecision(t, out); d != "allow" || reason != "tapped" {
		t.Fatalf("remote allow: %s (%s)", d, reason)
	}
	if len(fb.auth) != 1 || fb.auth[0] != "Bearer s3cr3t" {
		t.Fatalf("bridge must be called with the bearer secret: %v", fb.auth)
	}
}

func TestDecide(t *testing.T) {
	ctx := context.Background()
	req := hook.Request{Agent: "claude", Tool: "Bash", Command: "npm publish"}

	for verdict, want := range map[string]policy.Verdict{"allow": policy.Allow, "deny": policy.Deny, "ask": policy.Ask} {
		fb := newFakeBridge(t, verdict)
		if v, reason := decide(ctx, fb.config("x"), req, false); v != want {
			t.Errorf("bridge says %s: got %v (%s)", verdict, v, reason)
		}
	}

	// Fatal commands never reach the bridge, whatever it would answer.
	fb := newFakeBridge(t, "allow")
	if v, _ := decide(ctx, fb.config("x"), hook.Request{Agent: "claude", Command: "git push --force"}, false); v != policy.Deny {
		t.Fatalf("fatal command: %v", v)
	}
	if len(fb.auth) != 0 {
		t.Fatalf("fatal command reached the bridge: %v", fb.auth)
	}

	// Non-shell tools skip the policy and go to a human.
	if v, _ := decide(ctx, fb.config("x"), hook.Request{Agent: "claude", Tool: "Edit", Summary: "Edit /w/a.go"}, false); v != policy.Allow {
		t.Fatalf("non-shell tool: %v", v)
	}

	// User patterns from the config are honored; a bad pattern asks instead of failing open.
	cfg := fb.config("x")
	cfg.Policy.Fatal = []string{`\bterraform\s+destroy\b`}
	if v, _ := decide(ctx, cfg, hook.Request{Command: "terraform destroy"}, false); v != policy.Deny {
		t.Fatalf("custom fatal pattern: %v", v)
	}
	cfg.Policy.Safe = []string{`(`}
	if v, reason := decide(ctx, cfg, req, false); v != policy.Ask || !strings.Contains(reason, "policy error") {
		t.Fatalf("invalid pattern: %v (%s)", v, reason)
	}
}

func TestStopHookNotifiesAndRefreshesIndex(t *testing.T) {
	_, pixPath := env(t)
	fb := newFakeBridge(t, "allow")
	writeConfig(t, map[string]any{"bridge": map[string]any{"addr": fb.addr(), "secret": "s3cr3t"}})

	in, _ := json.Marshal(map[string]string{"session_id": pixID, "cwd": "/work/pix", "transcript_path": pixPath,
		"hook_event_name": "Stop"})
	if _, err := run(t, string(in), "hook", "stop"); err != nil {
		t.Fatal(err)
	}
	if len(fb.notified) != 1 {
		t.Fatalf("notifications: %v", fb.notified)
	}
	text := fb.notified[0]
	for _, want := range []string{"✅ claude finished in /work/pix", "Pix webhook", "idempotência", "agyh resume aaaa1111"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notification is missing %q:\n%s", want, text)
		}
	}

	// The hook upserted the session, so it is searchable before any sync.
	st, err := store.Open(os.Getenv("AGYH_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if s, err := st.Resolve(context.Background(), "claude:"+pixID); err != nil || s.Title != "Pix webhook" {
		t.Fatalf("session not indexed by the stop hook: %+v %v", s, err)
	}

	// Bad input or no bridge never fails the agent.
	if _, err := run(t, "garbage", "hook", "stop"); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, map[string]any{"bridge": map[string]any{"addr": closedAddr(t), "secret": "s"}})
	if _, err := run(t, string(in), "hook", "stop"); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- sync, doctor, helpers

func TestSyncRequiresConfiguration(t *testing.T) {
	env(t)
	for _, args := range [][]string{{"sync"}, {"sync", "status"}, {"sync", "push"}, {"sync", "pull"}, {"sync", "restore"}} {
		if _, err := run(t, "", args...); err == nil || !strings.Contains(err.Error(), "sync is disabled") {
			t.Errorf("agyh %s: %v", strings.Join(args, " "), err)
		}
	}
}

func TestPrintSyncStatus(t *testing.T) {
	var buf bytes.Buffer
	if err := printSyncStatus(&buf, agsync.StatusReport{Endpoint: "https://s3.example", Bucket: "vault",
		LocalSessions: 5, RemoteSessions: 3, ToUpload: 2, ToDownload: 0}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"https://s3.example", "vault", "To upload:", "2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Last vault update") {
		t.Fatal("zero LastSynced must be hidden")
	}
	buf.Reset()
	_ = printSyncStatus(&buf, agsync.StatusReport{LastSynced: time.Now().Add(-time.Hour)})
	if !strings.Contains(buf.String(), "Last vault update") {
		t.Fatal("LastSynced not shown")
	}
}

func TestDoctor(t *testing.T) {
	env(t)
	fb := newFakeBridge(t, "allow")
	writeConfig(t, map[string]any{"bridge": map[string]any{"addr": fb.addr(), "secret": "s3cr3t"}})
	out := mustRun(t, "doctor")
	for _, want := range []string{"Agents", "✓ claude", "✗ agy", "✗ codex", "claude: 2 sessions",
		"✗ telegram", "✗ whatsapp", "✓ bridge running on " + fb.addr()} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor is missing %q:\n%s", want, out)
		}
	}
}

func TestRemoteStatus(t *testing.T) {
	env(t)
	ctx := context.Background()
	if s := remoteStatus(ctx); s != "" {
		t.Fatalf("no channels configured: %q", s)
	}
	writeConfig(t, map[string]any{"telegram": map[string]any{"token": "t", "chat_id": 42},
		"bridge": map[string]any{"addr": closedAddr(t), "secret": "s"}})
	if s := remoteStatus(ctx); !strings.Contains(s, "telegram ✓") || !strings.Contains(s, "bridge stopped") {
		t.Fatalf("telegram without bridge: %q", s)
	}
	fb := newFakeBridge(t, "allow")
	writeConfig(t, map[string]any{"telegram": map[string]any{"token": "t", "chat_id": 42},
		"bridge": map[string]any{"addr": fb.addr(), "secret": "s"}})
	if s := remoteStatus(ctx); !strings.Contains(s, "bridge running") {
		t.Fatalf("telegram with bridge: %q", s)
	}
}

func TestPrintHits(t *testing.T) {
	var buf bytes.Buffer
	_ = printHits(&buf, nil, true)
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Fatalf("nil hits as JSON: %q", buf.String())
	}
	buf.Reset()
	hits := []store.Hit{{Snippet: "…a [match]…", Projects: []string{"a", "b"}}}
	hits[0].Agent, hits[0].ID, hits[0].Title = "codex", "0123456789abcdef", strings.Repeat("x", 100)
	_ = printHits(&buf, hits, false)
	out := buf.String()
	if !strings.Contains(out, "01234567 ") || strings.Contains(out, "0123456789") ||
		!strings.Contains(out, "#a #b") || !strings.Contains(out, "[match]") || !strings.Contains(out, "…") {
		t.Fatalf("table output:\n%s", out)
	}
}

func TestHelpers(t *testing.T) {
	if got := joinArgs([]string{" pix ", "webhook "}); got != "pix  webhook" {
		t.Fatalf("joinArgs: %q", got)
	}
	if got := joinArgs(nil); got != "" {
		t.Fatalf("joinArgs(nil): %q", got)
	}
	if firstNonEmpty("", "", "b", "c") != "b" || firstNonEmpty() != "" {
		t.Fatal("firstNonEmpty")
	}
	if displayName(bridge.TgUser{Username: "eu"}) != "@eu" || displayName(bridge.TgUser{ID: 7}) != "user 7" {
		t.Fatal("displayName")
	}
}

func TestVersionFlag(t *testing.T) {
	out := mustRun(t, "--version")
	if !strings.Contains(out, version) {
		t.Fatalf("--version: %q", out)
	}
}
