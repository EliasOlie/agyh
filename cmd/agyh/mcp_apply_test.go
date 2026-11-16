package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliasOlie/agyh/internal/agentkit"
)

const applySecret = "sk-APPLY-SECRET"

func TestMCPApplyCommands(t *testing.T) {
	home, _ := env(t)
	t.Setenv("AGYH_AGY_CONFIG_DIR", "")
	claudeJSON := filepath.Join(home, ".claude.json")
	agyJSON := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	codexTOML := filepath.Join(home, ".codex", "config.toml")
	put(t, claudeJSON, `{"mcpServers":{"github":{"command":"gh-mcp","env":{"GITHUB_TOKEN":"`+applySecret+`"}},"mine":{"command":"claude-mine"}}}`)
	put(t, agyJSON, `{"mcpServers":{"janus":{"command":"janus"},"mine":{"command":"agy-mine"}}}`)
	put(t, codexTOML, "# my codex config\nmodel = \"gpt-5\"\n")
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)

	// Stand-in for the `claude mcp` CLI: edits the fixture like the real one.
	old := agentkit.RunCLI
	t.Cleanup(func() { agentkit.RunCLI = old })
	agentkit.RunCLI = func(name string, args ...string) ([]byte, error) {
		raw, _ := os.ReadFile(claudeJSON)
		doc := map[string]map[string]any{}
		json.Unmarshal(raw, &doc)
		switch args[1] {
		case "add-json":
			var e any
			json.Unmarshal([]byte(args[5]), &e)
			doc["mcpServers"][args[4]] = e
		case "remove":
			delete(doc["mcpServers"], args[4])
		default:
			return nil, errors.New("unexpected")
		}
		out, _ := json.Marshal(doc)
		return nil, os.WriteFile(claudeJSON, out, 0o600)
	}

	if _, err := run(t, "", "mcp", "apply", "github", "--to", "agy"); err == nil || !strings.Contains(err.Error(), "manage_agents") {
		t.Fatalf("opt-in: %v", err)
	}
	before, _ := os.ReadFile(agyJSON)
	out := mustRun(t, "mcp", "apply", "github", "--to", "agy,codex", "--dry-run")
	if after, _ := os.ReadFile(agyJSON); string(after) != string(before) || !strings.Contains(out, "would be added") {
		t.Fatalf("dry run wrote, or said nothing:\n%s", out)
	}

	writeConfig(t, map[string]any{"manage_agents": true})
	out = mustRun(t, "mcp", "apply", "github", "--to", "agy,codex")
	if !strings.Contains(out, "+ agy: github added (stdio gh-mcp; from claude user)") || !strings.Contains(out, "+ codex: github added") {
		t.Fatalf("apply:\n%s", out)
	}
	for _, p := range []string{agyJSON, codexTOML} {
		if b, _ := os.ReadFile(p); !strings.Contains(string(b), applySecret) {
			t.Fatalf("%s: the server must arrive with its token", p)
		}
	}
	if b, _ := os.ReadFile(codexTOML); !strings.HasPrefix(string(b), "# my codex config\nmodel = \"gpt-5\"\n") {
		t.Fatal("codex config not preserved")
	}
	if out := mustRun(t, "mcp", "apply", "github", "--to", "agy"); !strings.Contains(out, "= agy: github already up to date") {
		t.Fatalf("again:\n%s", out)
	}

	// Into Claude, through its CLI.
	if out := mustRun(t, "mcp", "apply", "janus", "--to", "claude"); !strings.Contains(out, "+ claude: janus added (stdio janus; from agy user)") {
		t.Fatalf("to claude:\n%s", out)
	}
	if b, _ := os.ReadFile(claudeJSON); !strings.Contains(string(b), `"janus"`) {
		t.Fatal("claude CLI not used")
	}

	// Same name, different config: the user picks; their own entry is never replaced.
	if _, err := run(t, "", "mcp", "apply", "mine", "--to", "codex"); err == nil || !strings.Contains(err.Error(), "configured differently") {
		t.Fatalf("ambiguous: %v", err)
	}
	out, err := run(t, "", "mcp", "apply", "mine", "--from", "claude", "--to", "agy")
	if err == nil || !strings.Contains(out, "✗ agy: a server named mine exists and was not added by agyh") {
		t.Fatalf("unmanaged: %v\n%s", err, out)
	}

	var links []struct{ Agent, Name, Status string }
	if err := json.Unmarshal([]byte(mustRun(t, "mcp", "links", "--json")), &links); err != nil || len(links) != 3 {
		t.Fatalf("links: %+v %v", links, err)
	}
	for _, l := range links {
		if l.Status != "ok" {
			t.Fatalf("link status: %+v", l)
		}
	}

	if out := mustRun(t, "mcp", "remove", "github", "--agent", "codex"); !strings.Contains(out, "- codex: github removed") {
		t.Fatalf("remove:\n%s", out)
	}
	if b, _ := os.ReadFile(codexTOML); string(b) != "# my codex config\nmodel = \"gpt-5\"\n" {
		t.Fatalf("codex config not restored exactly: %q", b)
	}

	// No command ever prints a secret.
	for _, args := range [][]string{{"mcp"}, {"mcp", "--json"}, {"mcp", "links", "--json"}, {"mcp", "apply", "github", "--to", "codex", "--dry-run"}} {
		if out := mustRun(t, args...); strings.Contains(out, applySecret) {
			t.Fatalf("agyh %s leaked the token", strings.Join(args, " "))
		}
	}
}
