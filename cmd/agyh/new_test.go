package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/store"
)

// fakeAgents puts executable stand-ins for the given agent CLIs on PATH.
func fakeAgents(t *testing.T, names ...string) {
	t.Helper()
	bin := t.TempDir()
	for _, n := range names {
		os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	t.Setenv("PATH", bin)
}

func TestNewStartsAnAgentAndJoinsTheProject(t *testing.T) {
	home, _ := env(t)
	fakeAgents(t, "claude", "agy")
	os.MkdirAll(filepath.Join(home, "work", "api"), 0o755)
	// Started through a symlink (like macOS's /var -> /private/var): agyh must
	// run in, and record, the resolved directory, as the agent itself will.
	link := filepath.Join(home, "api-link")
	if err := os.Symlink(filepath.Join(home, "work", "api"), link); err != nil {
		t.Fatal(err)
	}
	ws, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	var gotBin string
	var gotArgv []string
	old := execFn
	t.Cleanup(func() { execFn = old })
	execFn = func(bin string, argv []string) error { gotBin, gotArgv = bin, argv; return nil }

	mustRun(t, "new", "agy", "-w", link, "fix", "the", "build")
	if !strings.HasSuffix(gotBin, "/agy") || strings.Join(gotArgv, "|") != "agy|-i|fix the build" {
		t.Fatalf("agy start: %s %q", gotBin, gotArgv)
	}
	if wd, _ := os.Getwd(); wd != ws {
		t.Fatalf("must start in the workspace, got %s", wd)
	}

	// claude, no prompt, inside a project: the session joins it once indexed.
	mustRun(t, "new", "-w", link, "-p", "pagamentos")
	if strings.Join(gotArgv, "|") != "claude" {
		t.Fatalf("claude start: %q", gotArgv)
	}
	writeSession(t, home, "-work-api", "bbbb3333-0000-4000-8000-000000000003", ws, "New API session", time.Now().UTC(), "hello", "hi")
	var hits []store.Hit
	if err := json.Unmarshal([]byte(mustRun(t, "ls", "-p", "pagamentos", "--json")), &hits); err != nil || len(hits) != 1 || hits[0].Title != "New API session" {
		t.Fatalf("the new session should have joined pagamentos: %+v %v", hits, err)
	}

	// Errors that explain themselves.
	if _, err := run(t, "", "new", "codex"); err == nil || !strings.Contains(err.Error(), "codex is not installed") {
		t.Fatalf("missing agent: %v", err)
	}
	if _, err := run(t, "", "new", "-w", "@nope"); err == nil || !strings.Contains(err.Error(), `no configured workspace named "nope"`) {
		t.Fatalf("unknown workspace: %v", err)
	}
	if _, err := run(t, "", "new", "-w", filepath.Join(home, "missing")); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("missing dir: %v", err)
	}
	writeConfig(t, map[string]any{"workspaces": []string{ws}})
	mustRun(t, "new", "-w", "@api")
	if wd, _ := os.Getwd(); wd != ws {
		t.Fatalf("@api: %s", wd)
	}
}

func TestProjectColorCommand(t *testing.T) {
	env(t)
	mustRun(t, "project", "new", "pix")
	if out := mustRun(t, "project", "color", "pix", "Purple"); !strings.Contains(out, "pix is now purple") {
		t.Fatalf("color: %q", out)
	}
	var ps []store.Project
	json.Unmarshal([]byte(mustRun(t, "project", "ls", "--json")), &ps)
	if len(ps) != 1 || ps[0].Color != "purple" {
		t.Fatalf("stored color: %+v", ps)
	}
	if out := mustRun(t, "project", "ls"); strings.Contains(out, "\x1b[") || !strings.Contains(out, "● pix") {
		t.Fatalf("plain output when not a terminal: %q", out)
	}
	if _, err := run(t, "", "project", "color", "pix", "chartreuse"); err == nil || !strings.Contains(err.Error(), "unknown color") {
		t.Fatalf("bad color: %v", err)
	}
	if _, err := run(t, "", "project", "color", "nope", "red"); err == nil || !strings.Contains(err.Error(), "no project named nope") {
		t.Fatalf("missing project: %v", err)
	}
	if out := mustRun(t, "project", "color", "pix", "auto"); !strings.Contains(out, "colored automatically") {
		t.Fatalf("auto: %q", out)
	}
}
