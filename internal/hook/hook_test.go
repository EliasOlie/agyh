package hook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EliasOlie/agyh/internal/policy"
)

func TestParseClaude(t *testing.T) {
	r, err := Parse("claude", strings.NewReader(`{"session_id":"s1","transcript_path":"/t.jsonl","cwd":"/w",
		"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"npm test"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionID != "s1" || r.Command != "npm test" || r.Workspace != "/w" || !r.IsShell() {
		t.Fatalf("unexpected: %+v", r)
	}
	r, _ = Parse("claude", strings.NewReader(`{"tool_name":"Edit","tool_input":{"file_path":"/w/a.go"}}`))
	if r.IsShell() || r.Describe() != "Edit /w/a.go" {
		t.Fatalf("non-shell tool: %+v", r)
	}
}

func TestParseAgy(t *testing.T) {
	r, err := Parse("agy", strings.NewReader(`{"conversationId":"c1","workspacePaths":["/w"],
		"toolCall":{"args":{"CommandLine":"git push"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionID != "c1" || r.Command != "git push" || r.Workspace != "/w" || r.Tool != "run_command" {
		t.Fatalf("unexpected: %+v", r)
	}
	if _, err := Parse("other", strings.NewReader(`{}`)); err == nil {
		t.Fatal("unknown agent accepted")
	}
	if _, err := Parse("claude", strings.NewReader(`not json`)); err == nil {
		t.Fatal("bad json accepted")
	}
}

func TestOutput(t *testing.T) {
	raw := Output("claude", policy.Ask, "why")
	var m map[string]map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["hookSpecificOutput"]["permissionDecision"] != "ask" || m["hookSpecificOutput"]["hookEventName"] != "PreToolUse" {
		t.Fatalf("claude output: %s", raw)
	}
	var a map[string]string
	json.Unmarshal(Output("agy", policy.Ask, "no answer"), &a)
	if a["decision"] != "deny" {
		t.Fatalf("agy has no ask: must deny, got %v", a)
	}
	json.Unmarshal(Output("agy", policy.Allow, ""), &a)
	if a["decision"] != "allow" {
		t.Fatalf("agy allow: %v", a)
	}
}

func TestPermissionRequestParsingAndOutput(t *testing.T) {
	r, err := Parse("claude", strings.NewReader(`{"session_id":"s","cwd":"/w","permission_mode":"auto","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"npm publish"}}`))
	if err != nil || r.PermissionMode != "auto" || !r.SelfApproving() || r.Command != "npm publish" {
		t.Fatalf("parse: %+v %v", r, err)
	}
	for mode, self := range map[string]bool{"default": false, "plan": false, "acceptEdits": false, "auto": true, "dontAsk": true, "bypassPermissions": true, "": false} {
		if (Request{PermissionMode: mode}).SelfApproving() != self {
			t.Errorf("mode %q", mode)
		}
	}
	var out struct {
		H struct {
			Event    string `json:"hookEventName"`
			Decision struct {
				Behavior, Message string
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	json.Unmarshal(PermissionOutput(policy.Allow, ""), &out)
	if out.H.Event != "PermissionRequest" || out.H.Decision.Behavior != "allow" {
		t.Fatalf("allow: %+v", out)
	}
	json.Unmarshal(PermissionOutput(policy.Deny, "no"), &out)
	if out.H.Decision.Behavior != "deny" || out.H.Decision.Message != "no" {
		t.Fatalf("deny: %+v", out)
	}
	if PermissionOutput(policy.Ask, "") != nil {
		t.Fatal("ask must print nothing so Claude shows its own prompt")
	}
}
