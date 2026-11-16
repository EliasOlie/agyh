package agentkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const mcpSecret = "sk-MCP-WRITE-SECRET"

func entry(agent, format, name, raw string) MCPEntry {
	return MCPEntry{Agent: agent, Scope: ScopeUser, Name: name, Format: format, Raw: json.RawMessage(raw)}
}

func TestSpecFromEntry(t *testing.T) {
	for name, c := range map[string]struct {
		e        MCPEntry
		want     MCPSpec
		warnings int
	}{
		"claude stdio": {entry("claude", "json", "gh", `{"type":"stdio","command":"gh-mcp","args":["--x"],"env":{"T":"v"}}`),
			MCPSpec{Transport: "stdio", Command: "gh-mcp", Args: []string{"--x"}, Env: map[string]string{"T": "v"}}, 0},
		"claude sse": {entry("claude", "json", "r", `{"type":"sse","url":"https://h/s","headers":{"A":"b"}}`),
			MCPSpec{Transport: "sse", URL: "https://h/s", Headers: map[string]string{"A": "b"}}, 0},
		"agy serverUrl": {entry("agy", "json", "j", `{"serverUrl":"https://j/mcp","disabled":true}`),
			MCPSpec{Transport: "http", URL: "https://j/mcp"}, 0},
		"codex url": {entry("codex", "toml", "f", `{"url":"https://f/mcp","bearer_token_env_var":"TOK","env_http_headers":{"X-Team":"TEAM"},"tool_timeout_sec":30}`),
			MCPSpec{Transport: "http", URL: "https://f/mcp", Headers: map[string]string{"Authorization": "Bearer ${TOK}", "X-Team": "${TEAM}"}}, 2},
		"codex stdio": {entry("codex", "toml", "g", `{"command":"gh-mcp","env":{"T":"v"},"enabled":true}`),
			MCPSpec{Transport: "stdio", Command: "gh-mcp", Env: map[string]string{"T": "v"}}, 0},
	} {
		got, warnings, err := SpecFromEntry(c.e)
		if err != nil || got.Digest() != c.want.Digest() || len(warnings) != c.warnings {
			t.Errorf("%s: got %+v warnings %v err %v", name, got, warnings, err)
		}
	}
	if _, _, err := SpecFromEntry(entry("x", "json", "n", `{"type":"stdio"}`)); err == nil {
		t.Error("entry without command or url accepted")
	}
}

func TestAgyWriteMCPPreservesTheRestOfTheFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	orig := `{"mcpServers":{"janus":{"command":"janus","disabled":true}},"otherSetting":{"keep":[1,2,3]}}`
	write(t, path, orig)
	os.Chmod(path, 0o640)
	a := NewAgy(home)
	t.Setenv("AGYH_AGY_CONFIG_DIR", "")
	if err := a.WriteMCP("gh", MCPSpec{Transport: "stdio", Command: "gh-mcp", Env: map[string]string{"T": mcpSecret}}, false); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers   map[string]map[string]any `json:"mcpServers"`
		OtherSetting json.RawMessage           `json:"otherSetting"`
	}
	raw, _ := os.ReadFile(path)
	json.Unmarshal(raw, &doc)
	var compact bytes.Buffer
	json.Compact(&compact, doc.OtherSetting)
	if compact.String() != `{"keep":[1,2,3]}` || doc.MCPServers["janus"]["disabled"] != true || doc.MCPServers["gh"]["command"] != "gh-mcp" {
		t.Fatalf("file not preserved:\n%s", raw)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed: %v", st.Mode())
	}
	if b, _ := os.ReadFile(path + ".agyh-backup"); string(b) != orig {
		t.Fatal("backup of the previous file missing")
	}
	a.RemoveMCP("gh")
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), `"gh"`) || !strings.Contains(string(raw), "janus") {
		t.Fatalf("remove:\n%s", raw)
	}
}

func TestCodexWriteMCPAppendsAndRemovesOnlyItsBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", "")
	path := filepath.Join(home, ".codex", "config.toml")
	orig := "# my settings, with comments\nmodel = \"gpt-5\"   # keep this comment\n\n[mcp_servers.mine]\ncommand = \"mine\"\n"
	write(t, path, orig)
	c := NewCodex(home)
	spec := MCPSpec{Transport: "http", URL: "https://f/mcp", Headers: map[string]string{"X-Key": mcpSecret}}
	if err := c.WriteMCP("figma", spec, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), orig) || !strings.Contains(string(raw), "# agyh:begin mcp figma") {
		t.Fatalf("existing content must be kept byte for byte:\n%s", raw)
	}
	got, ok, err := currentSpec(c, "figma")
	if err != nil || !ok || got.Digest() != spec.Digest() {
		t.Fatalf("read back: %+v %v %v", got, ok, err)
	}
	if err := c.RemoveMCP("figma"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != orig {
		t.Fatalf("remove must restore the file exactly:\n%q", raw)
	}
	if err := c.RemoveMCP("mine"); err == nil {
		t.Fatal("removed a server agyh did not write")
	}
}

// fakeClaudeCLI stands in for `claude mcp add-json/remove`, editing the
// fixture's .claude.json the way the real CLI would.
func fakeClaudeCLI(t *testing.T, cfgJSON string) *[][]string {
	t.Helper()
	var calls [][]string
	old := RunCLI
	t.Cleanup(func() { RunCLI = old })
	RunCLI = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		raw, _ := os.ReadFile(cfgJSON)
		doc := map[string]any{}
		json.Unmarshal(raw, &doc)
		servers, _ := doc["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		switch {
		case len(args) == 6 && args[1] == "add-json":
			var e any
			json.Unmarshal([]byte(args[5]), &e)
			servers[args[4]] = e
		case len(args) == 5 && args[1] == "remove":
			delete(servers, args[4])
		default:
			return []byte("error: bad call " + strings.Join(args, " ")), errors.New("exit status 1")
		}
		doc["mcpServers"] = servers
		out, _ := json.Marshal(doc)
		return nil, os.WriteFile(cfgJSON, out, 0o600)
	}
	return &calls
}

func TestMCPLedgerRules(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cfgJSON := filepath.Join(home, ".claude.json")
	write(t, cfgJSON, `{"numStartups":7,"mcpServers":{"mine":{"command":"mine"}}}`)
	calls := fakeClaudeCLI(t, cfgJSON)
	claude := NewClaude(home)
	ledger, _ := LoadLedger(filepath.Join(home, "data", "links.json"))
	spec := MCPSpec{Transport: "stdio", Command: "gh-mcp", Env: map[string]string{"GITHUB_TOKEN": mcpSecret}}

	plan := func(name string, s MCPSpec) MCPStep {
		t.Helper()
		st, err := ledger.PlanMCP(name, s, claude, "agy user")
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	st := plan("github", spec)
	if st.Kind != StepInstall {
		t.Fatalf("plan: %+v", st)
	}
	if out, _ := json.Marshal(st); strings.Contains(string(out), mcpSecret) {
		t.Fatal("a plan must not expose secrets")
	}
	if err := ledger.ApplyMCP(st); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; !slices.Equal(c[:5], []string{"claude", "mcp", "add-json", "--scope", "user"}) || c[5] != "github" {
		t.Fatalf("claude CLI call: %q", c)
	}
	if raw, _ := os.ReadFile(cfgJSON); !strings.Contains(string(raw), `"numStartups":7`) {
		t.Fatal("other state lost")
	}
	if st := plan("github", spec); st.Kind != StepUpToDate {
		t.Fatalf("again: %+v", st)
	}

	// The user's own server with the same name is never replaced.
	if st := plan("mine", spec); st.Kind != StepUnmanaged {
		t.Fatalf("unmanaged: %+v", st)
	}
	if st := plan("mine", MCPSpec{Transport: "stdio", Command: "mine"}); st.Kind != StepPresent {
		t.Fatalf("identical unmanaged: %+v", st)
	}

	// Source changed: agyh's untouched entry is updated.
	spec2 := spec
	spec2.Args = []string{"--v2"}
	st = plan("github", spec2)
	if st.Kind != StepUpdate {
		t.Fatalf("update: %+v", st)
	}
	if err := ledger.ApplyMCP(st); err != nil {
		t.Fatal(err)
	}

	// The user edits it: agyh leaves it alone, for update and removal.
	raw, _ := os.ReadFile(cfgJSON)
	write(t, cfgJSON, strings.Replace(string(raw), "--v2", "--mine", 1))
	if st := plan("github", spec); st.Kind != StepModified {
		t.Fatalf("edited, update: %+v", st)
	}
	link, _ := ledger.findMCP("claude", "github")
	if st, _ := ledger.PlanMCPRemove(link, claude); st.Kind != StepModified {
		t.Fatalf("edited, remove: %+v", st)
	}

	// Restore agyh's version, then remove it.
	write(t, cfgJSON, string(raw))
	st, _ = ledger.PlanMCPRemove(link, claude)
	if st.Kind != StepRemove {
		t.Fatalf("remove: %+v", st)
	}
	if err := ledger.ApplyMCP(st); err != nil || len(ledger.MCP) != 0 {
		t.Fatalf("remove: %v %+v", err, ledger.MCP)
	}
}

// An sse server lands in Antigravity as http: that must not look like a user edit.
func TestMCPNormalizationIsNotAnEdit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGYH_AGY_CONFIG_DIR", "")
	agy := NewAgy(home)
	ledger, _ := LoadLedger(filepath.Join(home, "links.json"))
	spec := MCPSpec{Transport: "sse", URL: "https://h/sse"}
	st, err := ledger.PlanMCP("remote", spec, agy, "claude user")
	if err != nil || st.Kind != StepInstall {
		t.Fatalf("%+v %v", st, err)
	}
	if err := ledger.ApplyMCP(st); err != nil {
		t.Fatal(err)
	}
	if st, _ := ledger.PlanMCP("remote", spec, agy, "claude user"); st.Kind != StepUpToDate {
		t.Fatalf("normalized write seen as %s", st.Kind)
	}
}

func TestClaudeCLIErrorsDoNotLeakSecrets(t *testing.T) {
	old := RunCLI
	t.Cleanup(func() { RunCLI = old })
	RunCLI = func(name string, args ...string) ([]byte, error) {
		return []byte("Invalid JSON: " + args[len(args)-1]), errors.New("exit status 1")
	}
	err := NewClaude(t.TempDir()).WriteMCP("x", MCPSpec{Transport: "stdio", Command: "c", Env: map[string]string{"K": mcpSecret}}, false)
	if err == nil || strings.Contains(err.Error(), mcpSecret) {
		t.Fatalf("error leaks the secret: %v", err)
	}
}
