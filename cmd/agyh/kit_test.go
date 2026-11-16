package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliasOlie/agyh/internal/agentkit"
)

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func kitFixture(t *testing.T) {
	t.Helper()
	home, _ := env(t)
	t.Setenv("AGYH_AGY_DIR", filepath.Join(home, ".gemini", "antigravity-cli"))
	t.Setenv("AGYH_AGY_CONFIG_DIR", "")
	md := "---\nname: ponytail\ndescription: Lazy senior dev mode.\n---\nbody\n"
	put(t, filepath.Join(home, ".claude", "skills", "ponytail", "SKILL.md"), md)
	put(t, filepath.Join(home, ".claude", "skills", "seo", "SKILL.md"), "---\nname: seo\ndescription: SEO audits.\n---\n")
	put(t, filepath.Join(home, ".gemini", "config", "plugins", "ponytail", "skills", "ponytail", "SKILL.md"), md)
	put(t, filepath.Join(home, ".gemini", "antigravity-cli", "builtin", "skills", "bundled", "SKILL.md"), "---\nname: bundled\n---\n")
	put(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{
		"github":{"command":"gh-mcp","args":["--token","sk-CLISECRET"],"env":{"GITHUB_TOKEN":"sk-CLISECRET"}},
		"remote":{"type":"http","url":"https://mcp.example.com/sk-CLISECRET"}}}`)
	put(t, filepath.Join(home, ".gemini", "config", "mcp_config.json"), `{"mcpServers":{
		"github":{"command":"docker","args":["run","sk-CLISECRET"]}}}`)
}

func TestSkillsCommand(t *testing.T) {
	kitFixture(t)
	out := mustRun(t, "skills")
	for _, want := range []string{"SKILL", "CLAUDE", "AGY", "ponytail", "from ponytail", "seo", "2 skills · 1 in every agent · 0 with differing copies"} {
		if !strings.Contains(out, want) {
			t.Fatalf("skills is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CODEX") || strings.Contains(out, "bundled") {
		t.Fatalf("codex is not installed and builtin skills are hidden by default:\n%s", out)
	}
	if out := mustRun(t, "skills", "--all"); !strings.Contains(out, "bundled") || !strings.Contains(out, "builtin") {
		t.Fatalf("--all must include builtin skills:\n%s", out)
	}

	var groups []agentkit.SkillGroup
	if err := json.Unmarshal([]byte(mustRun(t, "skills", "--json")), &groups); err != nil || len(groups) != 2 {
		t.Fatalf("--json: %+v %v", groups, err)
	}
	if groups[0].Name != "ponytail" || len(groups[0].Installs) != 2 || groups[0].Versions != 1 {
		t.Fatalf("ponytail: %+v", groups[0])
	}

	if out := mustRun(t, "skills", "--agent", "agy"); strings.Contains(out, "seo") || !strings.Contains(out, "ponytail") {
		t.Fatalf("--agent agy:\n%s", out)
	}
	if out := mustRun(t, "skills", "--agent", "codex"); !strings.Contains(out, "No skills found.") {
		t.Fatalf("codex not installed: %q", out)
	}
	if out := strings.TrimSpace(mustRun(t, "skills", "--agent", "codex", "--json")); out != "[]" {
		t.Fatalf("empty --json: %q", out)
	}
	if _, err := run(t, "", "skills", "--agent", "cursor"); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("unknown agent: %v", err)
	}
	if out := mustRun(t, "skills", "--help"); !strings.Contains(out, "not been tested against a real Codex install") {
		t.Fatalf("help must disclose the untested Codex support:\n%s", out)
	}
}

func TestMCPCommandNeverPrintsSecrets(t *testing.T) {
	kitFixture(t)
	for _, args := range [][]string{{"mcp"}, {"mcp", "--json"}, {"mcp", "--agent", "claude", "--json"}} {
		out := mustRun(t, args...)
		if strings.Contains(out, "sk-CLISECRET") {
			t.Fatalf("agyh %s leaked a secret:\n%s", strings.Join(args, " "), out)
		}
	}
	out := mustRun(t, "mcp")
	for _, want := range []string{"SERVER", "github", "stdio docker | stdio gh-mcp", "differs between agents", "remote", "https://mcp.example.com"} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp is missing %q:\n%s", want, out)
		}
	}
	var groups []agentkit.MCPGroup
	if err := json.Unmarshal([]byte(mustRun(t, "mcp", "--json")), &groups); err != nil || len(groups) != 2 || !groups[0].Differs {
		t.Fatalf("--json: %+v %v", groups, err)
	}
	if groups[0].Servers[1].EnvNames[0] != "GITHUB_TOKEN" {
		t.Fatalf("env var names are listed: %+v", groups[0].Servers)
	}
}
