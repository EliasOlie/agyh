package agentkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Claude reads Claude Code's skills and MCP servers.
//
// Documented layout (code.claude.com/docs): $CLAUDE_CONFIG_DIR or ~/.claude
// holds skills/<name>/SKILL.md, settings.json (enabledPlugins) and plugins/
// ($CLAUDE_CODE_PLUGIN_CACHE_DIR when set). .claude.json sits next to it, or
// at ~/.claude.json. plugins/marketplaces/ is the marketplace *catalog*, not
// what is installed, so it is never scanned.
type Claude struct {
	cfg, cfgJSON, plugins string
}

func NewClaude(home string) *Claude {
	cfg := filepath.Join(home, ".claude")
	cfgJSON := filepath.Join(home, ".claude.json")
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		cfg, cfgJSON = v, filepath.Join(v, ".claude.json")
	}
	return &Claude{cfg: cfg, cfgJSON: cfgJSON, plugins: envOr("CLAUDE_CODE_PLUGIN_CACHE_DIR", filepath.Join(cfg, "plugins"))}
}

func (c *Claude) Name() string    { return "claude" }
func (c *Claude) Available() bool { return exists(c.cfg) || fileExists(c.cfgJSON) }

func (c *Claude) Skills() ([]Skill, []error) {
	var (
		out  []Skill
		errs []error
	)
	add := func(s []Skill, e []error) { out, errs = append(out, s...), append(errs, e...) }

	skillsDir := filepath.Join(c.cfg, "skills")
	// A folder in skills/ may itself be a plugin (id "<name>@skills-dir").
	user, e := scanSkills(skillsDir, "claude", ScopeUser, "")
	add(user, e)
	if entries, err := os.ReadDir(skillsDir); err == nil {
		for _, ent := range entries {
			dir := filepath.Join(skillsDir, ent.Name())
			switch {
			case ent.Name() == "synced":
				// Skills synced down from claude.ai.
				s, e := scanSkills(dir, "claude", ScopeUser, "claude.ai")
				add(s, e)
			case strings.HasPrefix(ent.Name(), "."):
			case fileExists(filepath.Join(dir, ".claude-plugin", "plugin.json")):
				add(pluginSkills(dir, "claude", ent.Name()+"@skills-dir"))
			}
		}
	}

	enabled := c.enabledPlugins()
	for _, p := range c.installedPlugins(&errs) {
		s, e := pluginSkills(p.path, "claude", p.id)
		if !pluginEnabled(enabled, p.id, p.path) {
			for i := range s {
				s[i].Disabled = true
			}
		}
		add(s, e)
	}
	// Plugins synced from claude.ai are not in installed_plugins.json.
	if entries, err := os.ReadDir(filepath.Join(c.plugins, "synced")); err == nil {
		for _, ent := range entries {
			if ent.IsDir() && !strings.HasPrefix(ent.Name(), ".") {
				add(pluginSkills(filepath.Join(c.plugins, "synced", ent.Name()), "claude", ent.Name()+"@synced"))
			}
		}
	}
	return out, errs
}

type installedPlugin struct{ id, path, version string }

// installedPlugins reads plugins/installed_plugins.json. Only the field names
// (installPath, scope, version) are documented, not the file's shape, so any
// object with an installPath is taken, keyed by the nearest "name@market" key.
func (c *Claude) installedPlugins(errs *[]error) []installedPlugin {
	path := filepath.Join(c.plugins, "installed_plugins.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		*errs = append(*errs, err)
		return nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", shortHome(path), err))
		return nil
	}
	var out []installedPlugin
	var walk func(v any, id string)
	walk = func(v any, id string) {
		switch t := v.(type) {
		case map[string]any:
			if p, ok := t["installPath"].(string); ok && p != "" {
				if id == "" {
					id = filepath.Base(p)
				}
				v, _ := t["version"].(string)
				out = append(out, installedPlugin{id: id, path: p, version: v})
				return
			}
			for k, child := range t {
				childID := id
				if strings.Contains(k, "@") {
					childID = k
				}
				walk(child, childID)
			}
		case []any:
			for _, child := range t {
				walk(child, id)
			}
		}
	}
	walk(doc, "")
	slices.SortFunc(out, func(a, b installedPlugin) int { return strings.Compare(a.id+a.path, b.id+b.path) })
	return slices.CompactFunc(out, func(a, b installedPlugin) bool { return a == b })
}

// enabledPlugins reads enabledPlugins from the user settings.
func (c *Claude) enabledPlugins() map[string]bool {
	var s struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if raw, err := os.ReadFile(filepath.Join(c.cfg, "settings.json")); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s.EnabledPlugins
}

// pluginEnabled: an explicit enabledPlugins entry wins; otherwise the
// manifest's defaultEnabled, which defaults to true.
func pluginEnabled(enabled map[string]bool, id, dir string) bool {
	if v, ok := enabled[id]; ok {
		return v
	}
	var m struct {
		DefaultEnabled *bool `json:"defaultEnabled"`
	}
	if raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json")); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	return m.DefaultEnabled == nil || *m.DefaultEnabled
}

// pluginSkills lists a plugin's skills/<name>/SKILL.md, or its single root SKILL.md.
func pluginSkills(dir, agent, plugin string) ([]Skill, []error) {
	s, errs := scanSkills(filepath.Join(dir, "skills"), agent, ScopePlugin, plugin)
	if len(s) == 0 && fileExists(filepath.Join(dir, "SKILL.md")) {
		one, err := readSkill(dir)
		if err != nil {
			return nil, append(errs, fmt.Errorf("%s: %w", shortHome(dir), err))
		}
		one.Agent, one.Scope, one.Plugin = agent, ScopePlugin, plugin
		s = append(s, one)
	}
	return s, errs
}

// claudeMCP is the part of .claude.json that holds MCP servers.
type claudeMCP struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	} `json:"projects"`
}

func (c *Claude) readMCP() (*claudeMCP, error) {
	raw, err := os.ReadFile(c.cfgJSON)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc claudeMCP
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", shortHome(c.cfgJSON), err)
	}
	return &doc, nil
}

func (c *Claude) MCPConfig() ([]MCPEntry, []error) {
	doc, err := c.readMCP()
	if err != nil || doc == nil {
		return nil, errList(err)
	}
	var out []MCPEntry
	for name, raw := range doc.MCPServers {
		out = append(out, MCPEntry{Agent: "claude", Scope: ScopeUser, Name: name, Format: "json", Raw: raw})
	}
	for project, p := range doc.Projects {
		for name, raw := range p.MCPServers {
			out = append(out, MCPEntry{Agent: "claude", Scope: ScopeLocal, Project: project, Name: name, Format: "json", Raw: raw})
		}
	}
	return out, nil
}

func (c *Claude) Plugins() ([]PluginRef, []error) {
	var errs []error
	sources := c.marketplaceSources()
	var out []PluginRef
	for _, p := range c.installedPlugins(&errs) {
		ref := PluginRef{Agent: "claude", ID: p.id, Version: p.version}
		if _, mkt, ok := strings.Cut(p.id, "@"); ok {
			ref.Source = sources[mkt]
		}
		out = append(out, ref)
	}
	return out, errs
}

// marketplaceSources maps marketplace name -> where it comes from, from
// plugins/known_marketplaces.json ({"<name>": {"source": {...}}}).
func (c *Claude) marketplaceSources() map[string]string {
	raw, err := os.ReadFile(filepath.Join(c.plugins, "known_marketplaces.json"))
	if err != nil {
		return nil
	}
	var doc map[string]struct {
		Source map[string]any `json:"source"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := map[string]string{}
	for name, m := range doc {
		kind, _ := m.Source["source"].(string)
		for _, k := range []string{"repo", "url", "path"} {
			if v, ok := m.Source[k].(string); ok && v != "" {
				out[name] = strings.TrimPrefix(kind+":", ":") + v
				break
			}
		}
	}
	return out
}

func (c *Claude) MCPServers() ([]MCPServer, []error) {
	doc, err := c.readMCP()
	if err != nil || doc == nil {
		return nil, errList(err)
	}
	var (
		out  []MCPServer
		errs []error
	)
	add := func(servers map[string]json.RawMessage, scope, project string) {
		for name, entry := range servers {
			s, err := jsonMCPServer(name, entry)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: mcp server %q: %w", shortHome(c.cfgJSON), name, err))
				continue
			}
			s.Agent, s.Scope, s.Project, s.Config = "claude", scope, project, c.cfgJSON
			out = append(out, s)
		}
	}
	add(doc.MCPServers, ScopeUser, "")
	for project, p := range doc.Projects {
		add(p.MCPServers, ScopeLocal, project)
	}
	return out, errs
}
