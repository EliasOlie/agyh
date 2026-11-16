package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func skillFile(name, body string) string {
	return "---\nname: " + name + "\ndescription: " + body + "\n---\n" + body + "\n"
}

func TestSkillsLinkCommands(t *testing.T) {
	home, _ := env(t)
	t.Setenv("AGYH_AGY_CONFIG_DIR", "")
	t.Setenv("AGYH_SYNC_MACHINE", "this-box")
	agyPlugins := filepath.Join(home, ".gemini", "config", "plugins")
	put(t, filepath.Join(agyPlugins, "ponytail", "skills", "ponytail", "SKILL.md"), skillFile("ponytail", "Lazy mode."))
	put(t, filepath.Join(agyPlugins, "ponytail", "skills", "ponytail", "scripts", "run.sh"), "echo hi\n")
	put(t, filepath.Join(home, ".claude", "skills", "dup", "SKILL.md"), skillFile("dup", "Claude's version."))
	put(t, filepath.Join(agyPlugins, "other", "skills", "dup", "SKILL.md"), skillFile("dup", "Antigravity's version."))
	put(t, filepath.Join(agyPlugins, "other", "skills", "mine", "SKILL.md"), skillFile("mine", "From a plugin."))
	put(t, filepath.Join(home, ".agents", "skills", "mine", "SKILL.md"), skillFile("mine", "The user's own."))
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	claudeCopy := filepath.Join(home, ".claude", "skills", "ponytail", "SKILL.md")
	codexCopy := filepath.Join(home, ".agents", "skills", "ponytail", "SKILL.md")

	// Off by default: only a dry run is allowed, and it writes nothing.
	if _, err := run(t, "", "skills", "link", "ponytail", "--to", "claude"); err == nil || !strings.Contains(err.Error(), `"manage_agents": true`) {
		t.Fatalf("link without opt-in: %v", err)
	}
	out := mustRun(t, "skills", "link", "ponytail", "--to", "claude,codex", "--dry-run")
	if !strings.Contains(out, "would be installed in ~/.claude/skills/ponytail") || !strings.Contains(out, "nothing was written") {
		t.Fatalf("dry run:\n%s", out)
	}
	if _, err := os.Stat(claudeCopy); !os.IsNotExist(err) {
		t.Fatal("dry run wrote into an agent")
	}

	writeConfig(t, map[string]any{"manage_agents": true})
	out = mustRun(t, "skills", "link", "ponytail", "--to", "claude,codex")
	if !strings.Contains(out, "+ claude: ponytail installed in ~/.claude/skills/ponytail (from agy plugin ponytail)") ||
		!strings.Contains(out, "+ codex: ponytail installed in ~/.agents/skills/ponytail") {
		t.Fatalf("link:\n%s", out)
	}
	for _, p := range []string{claudeCopy, codexCopy} {
		if b, err := os.ReadFile(p); err != nil || !strings.Contains(string(b), "Lazy mode.") {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if out := mustRun(t, "skills", "link", "ponytail", "--to", "claude"); !strings.Contains(out, "= claude: ponytail already up to date") {
		t.Fatalf("relink:\n%s", out)
	}
	if out := mustRun(t, "skills"); !strings.Contains(out, "ponytail") || strings.Count(strings.SplitN(out[strings.Index(out, "ponytail"):], "\n", 2)[0], "user") != 2 {
		t.Fatalf("ponytail should now be a user skill in claude and codex:\n%s", out)
	}

	var links []struct {
		Agent, Name, Status string
	}
	if err := json.Unmarshal([]byte(mustRun(t, "skills", "links", "--json")), &links); err != nil || len(links) != 2 || links[0].Status != "ok" {
		t.Fatalf("links: %+v %v", links, err)
	}

	// Different versions with one name: the user must choose.
	if _, err := run(t, "", "skills", "link", "dup", "--to", "codex"); err == nil || !strings.Contains(err.Error(), "2 different versions") {
		t.Fatalf("ambiguous: %v", err)
	}
	if out := mustRun(t, "skills", "link", "dup", "--to", "codex", "--from", "agy"); !strings.Contains(out, "+ codex: dup installed") {
		t.Fatalf("--from agy:\n%s", out)
	}

	// The user's own folder with the same name is left alone, and the command says so.
	out, err := run(t, "", "skills", "link", "mine", "--to", "codex", "--from", "agy")
	if err == nil || !strings.Contains(out, "✗ codex: ~/.agents/skills/mine exists and was not created by agyh") {
		t.Fatalf("unmanaged: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".agents", "skills", "mine", "SKILL.md")); !strings.Contains(string(b), "The user's own.") {
		t.Fatal("user's skill was overwritten")
	}

	// Edited after install: unlink refuses; untouched copy: removed.
	put(t, filepath.Join(home, ".claude", "skills", "ponytail", "my-notes.md"), "tweak")
	if out, err := run(t, "", "skills", "unlink", "ponytail", "--agent", "claude"); err == nil || !strings.Contains(out, "was edited after agyh installed it") {
		t.Fatalf("unlink edited: %v\n%s", err, out)
	}
	if out := mustRun(t, "skills", "unlink", "ponytail", "--agent", "codex"); !strings.Contains(out, "- codex: ponytail removed") {
		t.Fatalf("unlink:\n%s", out)
	}
	if _, err := os.Stat(codexCopy); !os.IsNotExist(err) {
		t.Fatal("unlink left the copy")
	}
	if out := mustRun(t, "skills", "links"); !strings.Contains(out, "edited") || strings.Contains(out, "codex  ponytail") {
		t.Fatalf("links after unlink:\n%s", out)
	}

	// Errors that explain themselves.
	if _, err := run(t, "", "skills", "link", "ponytail", "--to", "agy", "--from", "agy"); err == nil || !strings.Contains(err.Error(), "Antigravity has no documented folder") {
		t.Fatalf("agy target: %v", err)
	}
	if _, err := run(t, "", "skills", "link", "nope", "--to", "claude"); err == nil || !strings.Contains(err.Error(), `no skill named "nope"`) {
		t.Fatalf("unknown skill: %v", err)
	}
	if _, err := run(t, "", "skills", "link", "ponytail", "--from", "agy"); err == nil || !strings.Contains(err.Error(), "--to") {
		t.Fatalf("missing --to: %v", err)
	}
	if _, err := run(t, "", "skills", "link", "--all"); err == nil || !strings.Contains(err.Error(), "--from") {
		t.Fatalf("--all without --from: %v", err)
	}
	if _, err := run(t, "", "skills", "unlink", "seo"); err == nil || !strings.Contains(err.Error(), "has not installed") {
		t.Fatalf("unlink unknown: %v", err)
	}

	// After a restore: every kit skill back into the agent it came from.
	kit := filepath.Join(os.Getenv("XDG_DATA_HOME"), "agyh", "kit", "old-laptop")
	put(t, filepath.Join(kit, "claude", "skills", "from-kit", "SKILL.md"), skillFile("from-kit", "Restored."))
	put(t, filepath.Join(kit, "codex", "skills", "codex-kit", "SKILL.md"), skillFile("codex-kit", "Restored too."))
	out = mustRun(t, "skills", "link", "--all", "--from", "kit:old-laptop")
	if !strings.Contains(out, "+ claude: from-kit installed") || !strings.Contains(out, "+ codex: codex-kit installed") || !strings.Contains(out, "kit:old-laptop") {
		t.Fatalf("kit link:\n%s", out)
	}
	if _, err := run(t, "", "skills", "link", "--all", "--from", "kit"); err == nil || !strings.Contains(err.Error(), "agyh sync restore --machine this-box") {
		t.Fatalf("missing kit: %v", err)
	}
}
