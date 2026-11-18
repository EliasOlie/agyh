package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EliasOlie/agyh/internal/store"
)

func permissionInput(mode, command string) string {
	b, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": "/w", "permission_mode": mode,
		"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	return string(b)
}

func behavior(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var v struct {
		H struct {
			Decision struct{ Behavior string } `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	return v.H.Decision.Behavior
}

func TestPermissionRequestHook(t *testing.T) {
	env(t)
	fb := newFakeBridge(t, "allow")
	writeConfig(t, map[string]any{"bridge": map[string]any{"addr": fb.addr(), "secret": "s3cr3t"}})
	run1 := func(mode, cmd string) string {
		out, err := run(t, permissionInput(mode, cmd), "hook", "permission-request")
		if err != nil {
			t.Fatal(err)
		}
		return behavior(t, out)
	}
	// Auto (and the other self-approving modes): agyh stays out, no phone.
	for _, mode := range []string{"auto", "dontAsk", "bypassPermissions"} {
		if b := run1(mode, "npm publish"); b != "" {
			t.Fatalf("%s mode must not decide, got %q", mode, b)
		}
	}
	if len(fb.auth) != 0 {
		t.Fatal("self-approving modes must not reach the phone")
	}
	if b := run1("default", "sudo rm -rf /"); b != "deny" {
		t.Fatalf("fatal: %q", b)
	}
	if b := run1("default", "git status"); b != "allow" {
		t.Fatalf("read-only: %q", b)
	}
	if b := run1("default", "npm publish"); b != "allow" || len(fb.auth) != 1 {
		t.Fatalf("approved on the phone: %q (bridge calls %d)", b, len(fb.auth))
	}
	fb.verdict = "deny"
	if b := run1("acceptEdits", "npm publish"); b != "deny" {
		t.Fatalf("denied on the phone: %q", b)
	}
	// No bridge: no decision, Claude shows its own prompt.
	writeConfig(t, map[string]any{"bridge": map[string]any{"addr": closedAddr(t), "secret": "s"}, "approval_timeout_sec": 2})
	if b := run1("default", "npm publish"); b != "" {
		t.Fatalf("no bridge must fall back to Claude's prompt, got %q", b)
	}
}

func TestPreToolUsePolicyOnly(t *testing.T) {
	env(t)
	fb := newFakeBridge(t, "allow")
	writeConfig(t, map[string]any{"bridge": map[string]any{"addr": fb.addr(), "secret": "s"}})
	for cmd, want := range map[string]string{"sudo rm -rf /": "deny", "git status": "allow", "npm publish": ""} {
		out, err := run(t, claudeHookInput(cmd), "hook", "pre-tool-use", "--agent", "claude", "--policy-only")
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if out != "" {
			got, _ = hookDecision(t, out)
		}
		if got != want {
			t.Errorf("%q: %q, want %q", cmd, got, want)
		}
	}
	if len(fb.auth) != 0 {
		t.Fatal("policy-only must never ask the bridge")
	}
	out := mustRun(t, "install-hooks", "claude")
	for _, want := range []string{"--policy-only", "agyh hook permission-request", "PermissionRequest", "agyh hook stop --agent claude"} {
		if !strings.Contains(out, want) {
			t.Fatalf("install-hooks claude is missing %q:\n%s", want, out)
		}
	}
}

func TestProjectMergeAndSimilarNames(t *testing.T) {
	env(t)
	mustRun(t, "project", "add", "conecta-me", "aaaa1111")
	if _, err := run(t, "", "project", "add", "conectame", "aaaa2222"); err == nil || !strings.Contains(err.Error(), `looks like the existing "conecta-me"`) {
		t.Fatalf("similar name on add: %v", err)
	}
	if _, err := run(t, "", "project", "new", "Conecta_Me"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("similar name on new: %v", err)
	}
	mustRun(t, "project", "add", "conectame", "aaaa2222", "--force")

	out := mustRun(t, "project", "merge", "conectame", "conecta-me")
	if !strings.Contains(out, "Moved 1 sessions from conectame into conecta-me") || !strings.Contains(out, "sessions are kept") {
		t.Fatalf("merge: %q", out)
	}
	var ps []store.Project
	json.Unmarshal([]byte(mustRun(t, "project", "ls", "--json")), &ps)
	if len(ps) != 1 || ps[0].Name != "conecta-me" || ps[0].Sessions != 2 {
		t.Fatalf("after merge: %+v", ps)
	}
	var hits []store.Hit
	json.Unmarshal([]byte(mustRun(t, "ls", "--json")), &hits)
	if len(hits) != 2 {
		t.Fatalf("sessions must survive a merge: %d", len(hits))
	}
	if out := mustRun(t, "project", "delete", "conecta-me"); !strings.Contains(out, "sessions are kept") {
		t.Fatalf("delete: %q", out)
	}
	json.Unmarshal([]byte(mustRun(t, "ls", "--json")), &hits)
	if len(hits) != 2 {
		t.Fatal("sessions must survive a delete")
	}
	if _, err := run(t, "", "project", "merge", "nope", "x"); err == nil || !strings.Contains(err.Error(), "no project named nope") {
		t.Fatalf("missing: %v", err)
	}
}
