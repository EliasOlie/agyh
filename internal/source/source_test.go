package source

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const claudeFixture = `{"type":"user","sessionId":"abc","cwd":"/home/me/proj","timestamp":"2026-11-01T10:00:00Z","message":{"role":"user","content":"Refatora o módulo de autenticação"}}
{"type":"user","isMeta":true,"timestamp":"2026-11-01T10:00:01Z","message":{"role":"user","content":"meta noise"}}
{"type":"user","timestamp":"2026-11-01T10:00:02Z","message":{"role":"user","content":"<command-name>/clear</command-name>"}}
{"type":"assistant","timestamp":"2026-11-01T10:01:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Feito: extraí o verificador de tokens."},{"type":"tool_use","name":"Bash"}]}}
{"type":"user","timestamp":"2026-11-01T10:01:30Z","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}
{"type":"assistant","isSidechain":true,"timestamp":"2026-11-01T10:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"subagent text"}]}}
{"type":"custom-title","customTitle":"Auth refactor","sessionId":"abc"}
{"type":"user","timestamp":"2026-11-01T10:03:00Z","message":{"role":"user","content":"partial line without end`

func TestClaude(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "-home-me-proj", "abc.jsonl"), claudeFixture)
	write(t, filepath.Join(root, "-home-me-proj", "abc", "subagents", "x.jsonl"), claudeFixture)
	c := NewClaude(root)
	refs, err := c.Refs(context.Background())
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs = %v, %v; want 1 top-level session", refs, err)
	}
	s, msgs, err := c.Load(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "abc" || s.Title != "Auth refactor" || s.Workspace != "/home/me/proj" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Text != "Feito: extraí o verificador de tokens." {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if s.Preview == "" || s.UpdatedAt.Before(s.StartedAt) {
		t.Fatalf("preview/times not set: %+v", s)
	}
	dir, argv := c.Resume(s)
	if dir != "/home/me/proj" || strings.Join(argv, " ") != "claude --resume abc" {
		t.Fatalf("resume = %s %v", dir, argv)
	}
}

const codexFixture = `{"timestamp":"2026-10-01T09:00:00Z","type":"session_meta","payload":{"id":"11111111-2222-3333-4444-555555555555","cwd":"/srv/app"}}
{"timestamp":"2026-10-01T09:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd</environment_context>"}]}}
{"timestamp":"2026-10-01T09:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"add retries to the queue consumer"}]}}
{"timestamp":"2026-10-01T09:00:03Z","type":"response_item","payload":{"type":"function_call","name":"shell"}}
{"timestamp":"2026-10-01T09:05:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Added exponential backoff."}]}}
`

func TestCodex(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "2026", "10", "01", "rollout-2026-10-01T09-00-00-11111111-2222-3333-4444-555555555555.jsonl"), codexFixture)
	c := NewCodex(root)
	refs, err := c.Refs(context.Background())
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs = %v, %v", refs, err)
	}
	s, msgs, err := c.Load(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "11111111-2222-3333-4444-555555555555" || s.Workspace != "/srv/app" || s.Title != "add retries to the queue consumer" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (noise dropped), got %+v", msgs)
	}
}

func TestAgy(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE conversation_summaries (conversation_id text PRIMARY KEY, title text NOT NULL DEFAULT "",
		preview text NOT NULL DEFAULT "", step_count integer NOT NULL DEFAULT 0, last_modified_time datetime NOT NULL,
		workspace_uris text NOT NULL, status text NOT NULL DEFAULT "", parent_conversation_id text NOT NULL DEFAULT "",
		last_user_input_time datetime NOT NULL);
		INSERT INTO conversation_summaries VALUES ('conv-1','Rotação de credenciais','prévia',12,
		'2026-11-08 11:13:24.89143277+00:00','file:///home/me/work/vpsql','CASCADE_RUN_STATUS_IDLE','','2026-11-08 11:10:00+00:00');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "brain", "conv-1", ".system_generated", "logs", "transcript.jsonl"),
		`{"type":"USER_INPUT","content":"<USER_REQUEST>\nimplementa a rotação\n</USER_REQUEST>\n<ADDITIONAL_METADATA>x</ADDITIONAL_METADATA>","created_at":"2026-11-08T11:00:00Z"}
{"type":"PLANNER_RESPONSE","content":"","created_at":"2026-11-08T11:00:05Z"}
{"type":"GENERIC","content":"tool output","created_at":"2026-11-08T11:00:06Z"}
{"type":"PLANNER_RESPONSE","content":"Pronto, rotação implementada.","created_at":"2026-11-08T11:05:00Z"}
`)
	a := NewAgy(root)
	if !a.Available() {
		t.Fatal("agy should be available")
	}
	refs, err := a.Refs(context.Background())
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs = %v, %v", refs, err)
	}
	s, msgs, err := a.Load(context.Background(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "Rotação de credenciais" || s.Workspace != "/home/me/work/vpsql" || s.Status != "IDLE" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if len(msgs) != 2 || msgs[0].Text != "implementa a rotação" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if s.UpdatedAt.IsZero() || s.StartedAt.IsZero() {
		t.Fatalf("times not parsed: %+v", s)
	}
}

func TestWorkspaceFromURIs(t *testing.T) {
	for in, want := range map[string]string{
		"file:///home/me/a":                     "/home/me/a",
		`["file:///home/me/b","file:///x"]`:     "/home/me/b",
		"file:///home/me/with%20space,file:///": "/home/me/with space",
		"":                                      "",
	} {
		if got := workspaceFromURIs(in); got != want {
			t.Errorf("workspaceFromURIs(%q) = %q, want %q", in, got, want)
		}
	}
}
