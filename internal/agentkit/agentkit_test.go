package agentkit

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Secrets planted in every fixture config: none may ever reach the output.
var secrets = []string{"sk-ARGSECRET", "ENVSECRET-value", "HEADERSECRET", "URLPATHSECRET", "QUERYSECRET", "TOMLSECRET"}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillMD(name, desc string) string {
	return "---\nname: " + name + "\ndescription: >\n  " + desc + "\n  second line\nlicense: MIT\n---\n\n# " + name + "\nBody.\n"
}

// fixture builds a home with Claude Code, Antigravity and Codex installs.
func fixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_PLUGIN_CACHE_DIR", "AGYH_AGY_CONFIG_DIR", "AGYH_AGY_DIR", "CODEX_HOME"} {
		t.Setenv(k, "")
	}
	t.Setenv("AGYH_CODEX_ADMIN_SKILLS", filepath.Join(home, "etc-codex-skills"))

	// ---- Claude Code
	c := filepath.Join(home, ".claude")
	write(t, filepath.Join(c, "skills", "ponytail", "SKILL.md"), skillMD("ponytail", "Lazy senior dev mode."))
	write(t, filepath.Join(c, "skills", "ponytail", "scripts", "run.sh"), "echo hi\n")
	write(t, filepath.Join(c, "skills", "no-frontmatter", "SKILL.md"), "# just a body\n")
	write(t, filepath.Join(c, "skills", "synced", "from-web", "SKILL.md"), skillMD("from-web", "Synced from claude.ai."))
	write(t, filepath.Join(c, "skills", ".trash", "gone", "SKILL.md"), skillMD("gone", "Deleted."))
	write(t, filepath.Join(c, "skills", "dirplug", ".claude-plugin", "plugin.json"), `{"name":"dirplug"}`)
	write(t, filepath.Join(c, "skills", "dirplug", "skills", "dir-skill", "SKILL.md"), skillMD("dir-skill", "In a skills-dir plugin."))
	// Marketplace catalog: available, NOT installed. Must never be listed.
	write(t, filepath.Join(c, "plugins", "marketplaces", "official", "plugins", "catalog", "skills", "catalog-skill", "SKILL.md"), skillMD("catalog-skill", "Not installed."))
	// Installed plugins: shape undocumented, so both a v2-like and a v1-like layout.
	enabledPlug := filepath.Join(c, "plugins", "cache", "mkt", "on", "1.0.0")
	disabledPlug := filepath.Join(c, "plugins", "cache", "mkt", "off", "2.0.0")
	defaultOffPlug := filepath.Join(c, "plugins", "cache", "other", "quiet", "0.1.0")
	write(t, filepath.Join(enabledPlug, "skills", "on-skill", "SKILL.md"), skillMD("on-skill", "Enabled plugin."))
	write(t, filepath.Join(disabledPlug, "skills", "off-skill", "SKILL.md"), skillMD("off-skill", "Disabled plugin."))
	write(t, filepath.Join(defaultOffPlug, ".claude-plugin", "plugin.json"), `{"name":"quiet","defaultEnabled":false}`)
	write(t, filepath.Join(defaultOffPlug, "SKILL.md"), skillMD("quiet-root", "Root-level single skill."))
	installed, _ := json.Marshal(map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"on@mkt":  []any{map[string]any{"scope": "user", "installPath": enabledPlug, "version": "1.0.0"}},
			"off@mkt": []any{map[string]any{"scope": "user", "installPath": disabledPlug}},
		},
		"legacy": map[string]any{"quiet@other": map[string]any{"installPath": defaultOffPlug}},
	})
	write(t, filepath.Join(c, "plugins", "installed_plugins.json"), string(installed))
	write(t, filepath.Join(c, "settings.json"), `{"enabledPlugins":{"on@mkt":true,"off@mkt":false}}`)
	write(t, filepath.Join(home, ".claude.json"), `{
		"numStartups": 3,
		"mcpServers": {
			"github": {"command": "/usr/local/bin/gh-mcp --token sk-ARGSECRET", "args": ["--key", "sk-ARGSECRET"], "env": {"GITHUB_TOKEN": "ENVSECRET-value"}},
			"remote": {"type": "http", "url": "https://mcp.example.com/v1/URLPATHSECRET?k=QUERYSECRET", "headers": {"Authorization": "Bearer HEADERSECRET"}},
			"templated": {"type": "http", "url": "${MCP_URL}"}
		},
		"projects": {"/work/api": {"mcpServers": {"db": {"command": "pg-mcp", "env": {"PGPASSWORD": "ENVSECRET-value"}}}}}
	}`)

	// ---- Antigravity: same ponytail skill (identical content) via a plugin.
	g := filepath.Join(home, ".gemini", "config")
	write(t, filepath.Join(g, "plugins", "ponytail", "skills", "ponytail", "SKILL.md"), skillMD("ponytail", "Lazy senior dev mode."))
	write(t, filepath.Join(g, "plugins", "ponytail", "skills", "ponytail", "scripts", "run.sh"), "echo hi\n")
	write(t, filepath.Join(g, "plugins", "ponytail", ".openclaw", "skills", "ponytail", "SKILL.md"), skillMD("ponytail", "Other agent's copy."))
	write(t, filepath.Join(g, "plugins", "design", "skills", "no-frontmatter", "SKILL.md"), "# a different body\n")
	write(t, filepath.Join(home, ".gemini", "antigravity-cli", "builtin", "skills", "bundled", "SKILL.md"), skillMD("bundled", "Ships with agy."))
	write(t, filepath.Join(g, "mcp_config.json"), `{"mcpServers": {
		"github": {"command": "gh-mcp", "args": ["sk-ARGSECRET"], "env": {"GITHUB_TOKEN": "ENVSECRET-value"}},
		"janus": {"serverUrl": "https://janus.example.org/mcp/URLPATHSECRET", "disabled": true}
	}}`)

	// ---- Codex (public docs; see codex.go)
	x := filepath.Join(home, ".codex")
	write(t, filepath.Join(home, ".agents", "skills", "ponytail", "SKILL.md"), skillMD("ponytail", "Lazy senior dev mode, edited."))
	write(t, filepath.Join(home, ".agents", "skills", "muted", "SKILL.md"), skillMD("muted", "Disabled by name."))
	write(t, filepath.Join(x, "skills", "legacy-path", "SKILL.md"), skillMD("legacy-path", "Old CODEX_HOME/skills location."))
	write(t, filepath.Join(home, "etc-codex-skills", "admin-skill", "SKILL.md"), skillMD("admin-skill", "Machine-wide."))
	write(t, filepath.Join(x, "config.toml"), fmt.Sprintf(`model = "gpt-5"

[[skills.config]]
name = "muted"
enabled = false

[[skills.config]]
path = %q
enabled = false

[[skills.config]]
path = %q
enabled = true

[mcp_servers.github]
command = "gh-mcp"
args = ["--token", "TOMLSECRET"]
env = { GITHUB_TOKEN = "TOMLSECRET" }

[mcp_servers.figma]
url = "https://mcp.figma.com/mcp/URLPATHSECRET"
bearer_token_env_var = "FIGMA_OAUTH_TOKEN"
http_headers = { "X-Key" = "HEADERSECRET" }
env_http_headers = { "X-Team" = "FIGMA_TEAM" }
enabled = false
`, filepath.Join(x, "skills", "legacy-path", "SKILL.md"), filepath.Join(x, "skills", "legacy-path", "SKILL.md")))
	return home
}

func collect(t *testing.T) ([]Skill, []MCPServer) {
	t.Helper()
	var (
		skills  []Skill
		servers []MCPServer
	)
	for _, h := range All() {
		if !h.Available() {
			t.Fatalf("%s not available in the fixture", h.Name())
		}
		s, errs := h.Skills()
		if len(errs) > 0 {
			t.Fatalf("%s skills: %v", h.Name(), errs)
		}
		m, errs := h.MCPServers()
		if len(errs) > 0 {
			t.Fatalf("%s mcp: %v", h.Name(), errs)
		}
		skills, servers = append(skills, s...), append(servers, m...)
	}
	return skills, servers
}

func find[T any](items []T, match func(T) bool) []T {
	var out []T
	for _, it := range items {
		if match(it) {
			out = append(out, it)
		}
	}
	return out
}

func TestSkillsAcrossAgents(t *testing.T) {
	fixture(t)
	skills, _ := collect(t)
	groups := GroupSkills(skills)
	by := map[string]SkillGroup{}
	for _, g := range groups {
		by[g.Name] = g
	}

	for _, absent := range []string{"catalog-skill", "gone"} {
		if _, ok := by[absent]; ok {
			t.Errorf("%s must not be listed", absent)
		}
	}

	p := by["ponytail"]
	if !slices.Equal(p.Agents(), []string{"agy", "claude", "codex"}) || len(p.Installs) != 3 {
		t.Fatalf("ponytail installs: %+v", p.Installs)
	}
	// Claude and agy copies are identical; codex's was edited.
	if p.Versions != 2 || p.Description != "Lazy senior dev mode. second line" {
		t.Fatalf("ponytail group: versions=%d desc=%q", p.Versions, p.Description)
	}
	if agy := find(p.Installs, func(s Skill) bool { return s.Agent == "agy" }); agy[0].Scope != ScopePlugin || agy[0].Plugin != "ponytail" {
		t.Fatalf("agy ponytail: %+v", agy)
	}

	if g := by["no-frontmatter"]; len(g.Installs) != 2 || g.Versions != 2 {
		t.Fatalf("name falls back to the folder; different bodies differ: %+v", g)
	}
	for name, want := range map[string]struct {
		agent, scope, plugin string
		disabled             bool
	}{
		"from-web":    {"claude", ScopeUser, "claude.ai", false},
		"dir-skill":   {"claude", ScopePlugin, "dirplug@skills-dir", false},
		"on-skill":    {"claude", ScopePlugin, "on@mkt", false},
		"off-skill":   {"claude", ScopePlugin, "off@mkt", true},     // enabledPlugins false
		"quiet-root":  {"claude", ScopePlugin, "quiet@other", true}, // manifest defaultEnabled false
		"bundled":     {"agy", ScopeBuiltin, "", false},
		"muted":       {"codex", ScopeUser, "", true},  // disabled by name
		"legacy-path": {"codex", ScopeUser, "", false}, // disabled by path, re-enabled later: last wins
		"admin-skill": {"codex", ScopeAdmin, "", false},
	} {
		g, ok := by[name]
		if !ok || len(g.Installs) != 1 {
			t.Errorf("%s: %+v", name, g)
			continue
		}
		s := g.Installs[0]
		if s.Agent != want.agent || s.Scope != want.scope || s.Plugin != want.plugin || s.Disabled != want.disabled {
			t.Errorf("%s: got %s/%s/%s disabled=%v, want %+v", name, s.Agent, s.Scope, s.Plugin, s.Disabled, want)
		}
	}
}

func TestMCPServersNeverExposeSecrets(t *testing.T) {
	fixture(t)
	_, servers := collect(t)
	out, _ := json.Marshal(GroupMCP(servers))
	for _, s := range secrets {
		if strings.Contains(string(out), s) {
			t.Fatalf("secret %q leaked:\n%s", s, out)
		}
	}

	key := func(s MCPServer) string { return s.Agent + "/" + s.Name + "/" + s.Scope }
	got := map[string]MCPServer{}
	for _, s := range servers {
		got[key(s)] = s
	}
	for k, want := range map[string]MCPServer{
		"claude/github/user":    {Transport: "stdio", Target: "gh-mcp", EnvNames: []string{"GITHUB_TOKEN"}},
		"claude/remote/user":    {Transport: "http", Target: "https://mcp.example.com"},
		"claude/templated/user": {Transport: "http", Target: "(templated url)"},
		"claude/db/local":       {Transport: "stdio", Target: "pg-mcp", EnvNames: []string{"PGPASSWORD"}, Project: "/work/api"},
		"agy/github/user":       {Transport: "stdio", Target: "gh-mcp", EnvNames: []string{"GITHUB_TOKEN"}},
		"agy/janus/user":        {Transport: "http", Target: "https://janus.example.org", Disabled: true},
		"codex/github/user":     {Transport: "stdio", Target: "gh-mcp", EnvNames: []string{"GITHUB_TOKEN"}},
		"codex/figma/user":      {Transport: "http", Target: "https://mcp.figma.com", EnvNames: []string{"FIGMA_OAUTH_TOKEN", "FIGMA_TEAM"}, Disabled: true},
	} {
		s, ok := got[k]
		if !ok {
			t.Errorf("%s missing", k)
			continue
		}
		if s.Transport != want.Transport || s.Target != want.Target || s.Disabled != want.Disabled ||
			s.Project != want.Project || !slices.Equal(s.EnvNames, want.EnvNames) {
			t.Errorf("%s: got %+v", k, s)
		}
	}
	if len(servers) != 8 {
		t.Errorf("got %d servers, want 8", len(servers))
	}

	for _, g := range GroupMCP(servers) {
		if g.Name == "github" && (len(g.Servers) != 3 || g.Differs) {
			t.Errorf("github is the same everywhere: %+v", g)
		}
	}
}

// Listing must not modify anything: agyh only reads agent files.
func TestInspectionIsReadOnly(t *testing.T) {
	home := fixture(t)
	snapshot := func() map[string]string {
		out := map[string]string{}
		filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				b, _ := os.ReadFile(p)
				st, _ := os.Stat(p)
				out[p] = fmt.Sprintf("%x %v %v", sha256.Sum256(b), st.ModTime().UnixNano(), st.Mode())
			}
			return nil
		})
		return out
	}
	before := snapshot()
	collect(t)
	after := snapshot()
	if len(before) != len(after) {
		t.Fatalf("files created or removed: %d -> %d", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Fatalf("%s changed", p)
		}
	}
}

func TestClaudeConfigDirRelocatesEverything(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, "elsewhere")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_CODE_PLUGIN_CACHE_DIR", "")
	write(t, filepath.Join(cfg, "skills", "moved", "SKILL.md"), skillMD("moved", "x"))
	write(t, filepath.Join(cfg, ".claude.json"), `{"mcpServers":{"m":{"command":"m"}}}`)
	write(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"ignored":{"command":"x"}}}`)
	c := NewClaude(home)
	s, _ := c.Skills()
	m, _ := c.MCPServers()
	if len(s) != 1 || s[0].Name != "moved" || len(m) != 1 || m[0].Name != "m" {
		t.Fatalf("skills %+v mcp %+v", s, m)
	}
}

func TestHashDir(t *testing.T) {
	base := t.TempDir()
	mk := func(name string, files map[string]string) string {
		dir := filepath.Join(base, name)
		for p, c := range files {
			write(t, filepath.Join(dir, p), c)
		}
		return dir
	}
	h := func(dir string) string {
		t.Helper()
		v, err := hashDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	a := mk("a", map[string]string{"SKILL.md": "x", "s/run.sh": "y"})
	b := mk("b", map[string]string{"SKILL.md": "x", "s/run.sh": "y", ".git/HEAD": "ref", "node_modules/m/i.js": "z",
		".venv/lib/p.py": "z", "__pycache__/c.pyc": "z", "ms-playwright/chromium/bin": "z"})
	if h(a) != h(b) {
		t.Fatal("VCS metadata and dependency folders must not change the fingerprint")
	}
	if c := mk("c", map[string]string{"SKILL.md": "x", "s/run.sh": "Y"}); h(a) == h(c) {
		t.Fatal("content change not detected")
	}
	if d := mk("d", map[string]string{"SKILL.md": "x", "s/other.sh": "y"}); h(a) == h(d) {
		t.Fatal("rename not detected")
	}
	e := mk("e", map[string]string{"SKILL.md": "x", "s/run.sh": "y"})
	os.Chmod(filepath.Join(e, "s", "run.sh"), 0o755)
	if h(a) == h(e) {
		t.Fatal("executable bit not part of the fingerprint")
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(a, link); err == nil && h(link) != h(a) {
		t.Fatal("a symlinked skill folder must hash like its target")
	}
}

func TestOversizeSkillIsStillListed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "huge")
	write(t, filepath.Join(dir, "SKILL.md"), skillMD("huge", "Too many files."))
	for i := range maxSkillFiles + 1 {
		write(t, filepath.Join(dir, "data", fmt.Sprintf("%d.txt", i)), "x")
	}
	s, errs := scanSkills(root, "claude", ScopeUser, "")
	if len(s) != 1 || s[0].Name != "huge" || s[0].Hash != "" || len(errs) != 1 {
		t.Fatalf("skills %+v errs %v", s, errs)
	}
	if g := GroupSkills(append(s, s[0])); g[0].Versions != 0 {
		t.Fatalf("unknown hashes are not versions: %+v", g[0])
	}
}

func TestFrontmatter(t *testing.T) {
	for in, want := range map[string]skillFrontmatter{
		"---\nname: a\ndescription: one line\n---\nbody":               {Name: "a", Description: "one line"},
		"\xef\xbb\xbf---\r\nname: bom\r\ndescription: crlf\r\n---\r\n": {Name: "bom", Description: "crlf"},
		"---\nname: f\ndescription: >\n  folded\n  text\n---\n":        {Name: "f", Description: "folded text"},
		"---\nname: [unclosed\n---\n":                                  {},
		"no frontmatter":                                               {},
		"---\nname: x\nno closing fence":                               {},
	} {
		if got := frontmatter([]byte(in)); got != want {
			t.Errorf("%q: got %+v want %+v", in, got, want)
		}
	}
}
