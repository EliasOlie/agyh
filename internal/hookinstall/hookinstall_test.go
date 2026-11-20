package hookinstall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPaths(t *testing.T) Paths {
	home := t.TempDir()
	return Paths{
		Home:      home,
		Claude:    filepath.Join(home, ".claude", "settings.json"),
		Agy:       filepath.Join(home, ".agents", "hooks.json"),
		AgyLegacy: filepath.Join(home, ".gemini", "config", "hooks.json"),
		Codex:     filepath.Join(home, ".codex", "hooks.json"),
	}
}

func put(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, b)
	}
	return m
}

func TestInstallKeepsOtherHooksAndSettings(t *testing.T) {
	p := testPaths(t)
	put(t, p.Claude, `{
  "permissions": {"allow": ["Bash(ls)"]},
  "hooks": {
    "PreToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "prettier --check"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "/usr/local/bin/agyh hook stop --agent claude"}]}]
  }
}`)
	res, err := Install(p, "claude", "/home/me/bin/agyh")
	if err != nil {
		t.Fatal(err)
	}
	if res.Backup == "" || res.Created {
		t.Fatalf("result: %+v", res)
	}
	m := load(t, p.Claude)
	if m["permissions"] == nil {
		t.Fatal("unrelated settings lost")
	}
	b, _ := json.Marshal(m["hooks"])
	s := string(b)
	for _, want := range []string{"prettier --check", "/home/me/bin/agyh hook stop --agent claude",
		"/home/me/bin/agyh hook permission-request --agent claude", "--policy-only"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	if strings.Contains(s, "/usr/local/bin/agyh") {
		t.Errorf("the previous agyh hook was not replaced: %s", s)
	}
	if fi, _ := os.Stat(p.Claude); fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode changed to %v", fi.Mode().Perm())
	}
}

func TestInstallCreatesCodexAndAgyFiles(t *testing.T) {
	p := testPaths(t)
	for _, agent := range []string{"codex", "agy"} {
		res, err := Install(p, agent, "agyh")
		if err != nil || !res.Created || res.Backup != "" {
			t.Fatalf("%s: %+v %v", agent, res, err)
		}
	}
	if !strings.Contains(string(mustRead(t, p.Codex)), "hook permission-request --agent codex") {
		t.Fatal("codex hooks missing")
	}
	agy := load(t, p.Agy)
	if agy["agyh-permission"] == nil || agy["agyh-stop"] == nil {
		t.Fatalf("agy hooks: %v", agy)
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The real case: the old Python bridge was removed, its hook was not, and
// Antigravity refused every command.
func TestStaleHooksFoundAndRemoved(t *testing.T) {
	p := testPaths(t)
	script := filepath.Join(p.Home, "scripts", "ok.sh")
	put(t, script, "#!/bin/sh\n")
	put(t, p.AgyLegacy, `{
  "telegram-pre-tool-permission": {"PreToolUse": [{"matcher": "run_command", "hooks": [
    {"type": "command", "command": "python3 `+p.Home+`/.agents/scripts/telegram_permission.py", "timeout": 55}]}]},
  "telegram-stop": {"Stop": [{"type": "command", "command": "python3 ~/.agents/scripts/telegram_stop.py"}]},
  "fine": {"Stop": [{"type": "command", "command": "bash `+script+`"}]},
  "on-path": {"Stop": [{"type": "command", "command": "notify-send done"}]},
  "shell": {"Stop": [{"type": "command", "command": "test -x /nope && /nope"}]}
}`)
	stale, err := FindStale(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 2 {
		t.Fatalf("want the two python hooks, got %+v", stale)
	}
	removed, backup, err := RemoveStale(p, p.AgyLegacy)
	if err != nil || len(removed) != 2 || backup == "" {
		t.Fatalf("remove: %+v %q %v", removed, backup, err)
	}
	m := load(t, p.AgyLegacy)
	if len(m) != 3 || m["fine"] == nil || m["on-path"] == nil || m["shell"] == nil {
		t.Fatalf("left: %v", m)
	}
	if again, _ := FindStale(p); len(again) != 0 {
		t.Fatalf("still stale: %+v", again)
	}
}

func TestInvalidJSONIsLeftAlone(t *testing.T) {
	p := testPaths(t)
	put(t, p.Claude, `{"hooks": `)
	if _, err := Install(p, "claude", "agyh"); err == nil {
		t.Fatal("wrote over an invalid settings file")
	}
	if string(mustRead(t, p.Claude)) != `{"hooks": ` {
		t.Fatal("invalid file was modified")
	}
}

func TestQuoteBinWithSpaces(t *testing.T) {
	s, _ := Snippet("agy", "/Users/Me Too/bin/agyh")
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `'/Users/Me Too/bin/agyh' hook`) {
		t.Fatalf("%s", b)
	}
}

// agyh updates its own copy of the skill, never one the user edited.
func TestSendSkill(t *testing.T) {
	p := testPaths(t)
	path, status, err := InstallSkill(p, "claude")
	if err != nil || status != "installed" || path != filepath.Join(p.Home, ".claude", "skills", "agyh-send", "SKILL.md") {
		t.Fatalf("%s %s %v", path, status, err)
	}
	if !strings.Contains(string(mustRead(t, path)), "agyh send") {
		t.Fatal("skill body")
	}
	if _, status, _ := InstallSkill(p, "claude"); status != "up to date" {
		t.Fatalf("second run: %s", status)
	}
	// An older agyh copy is updated…
	os.WriteFile(path, []byte("old agyh text"), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(path), ".agyh-digest"), []byte(digest([]byte("old agyh text"))+"\n"), 0o644)
	if _, status, _ := InstallSkill(p, "claude"); status != "updated" {
		t.Fatalf("own copy: %s", status)
	}
	// …a copy the user edited is not.
	os.WriteFile(path, append(mustRead(t, path), []byte("\nmy note\n")...), 0o644)
	if _, status, _ := InstallSkill(p, "claude"); !strings.HasPrefix(status, "kept") {
		t.Fatalf("edited copy: %s", status)
	}
	if !strings.Contains(string(mustRead(t, path)), "my note") {
		t.Fatal("user edit lost")
	}
	if path, _, _ := InstallSkill(p, "agy"); path != "" {
		t.Fatal("agy has no folder for user skills: nothing must be written")
	}
	if path, _, _ := InstallSkill(p, "codex"); !strings.HasSuffix(path, filepath.Join(".agents", "skills", "agyh-send", "SKILL.md")) {
		t.Fatalf("codex: %s", path)
	}
}
