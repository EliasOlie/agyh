// Package agentkit inspects what each coding agent has installed: Agent
// Skills (folders with a SKILL.md) and MCP servers. It is read-only: it never
// writes to an agent's files.
//
// Every agent keeps these in its own place and format. A Home hides that,
// the same way internal/source does for conversations.
package agentkit

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Scopes of an installed skill or MCP server.
const (
	ScopeUser    = "user"    // installed by the user for every project
	ScopePlugin  = "plugin"  // shipped inside an installed plugin
	ScopeBuiltin = "builtin" // bundled with the agent itself
	ScopeAdmin   = "admin"   // machine-wide (e.g. /etc)
	ScopeLocal   = "local"   // one project only (Claude Code "local" MCP scope)
)

// Home is one agent's installation on this machine.
type Home interface {
	Name() string
	// Available reports whether the agent's config exists on this machine.
	Available() bool
	Skills() ([]Skill, []error)
	MCPServers() ([]MCPServer, []error)
}

// All returns every supported agent home, rooted at the user's home directory.
func All() []Home {
	home, _ := os.UserHomeDir()
	return []Home{NewClaude(home), NewAgy(home), NewCodex(home)}
}

// Skill is one installed copy of a skill in one agent.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Agent       string `json:"agent"`
	Scope       string `json:"scope"`
	Plugin      string `json:"plugin,omitempty"` // plugin that ships it (ScopePlugin)
	Path        string `json:"path"`             // the skill folder
	Hash        string `json:"hash"`             // content fingerprint ("" if the folder was too large)
	Disabled    bool   `json:"disabled,omitempty"`
}

// MCPServer is one MCP server configured in one agent. It never holds secret
// material: no argument values, header values, env values or URL paths.
type MCPServer struct {
	Name      string   `json:"name"`
	Agent     string   `json:"agent"`
	Scope     string   `json:"scope"`
	Project   string   `json:"project,omitempty"` // ScopeLocal: the project path
	Transport string   `json:"transport"`         // stdio, http or sse
	Target    string   `json:"target"`            // command name (stdio) or URL host (http/sse)
	EnvNames  []string `json:"env,omitempty"`     // names of env vars it receives, never values
	Disabled  bool     `json:"disabled,omitempty"`
	Config    string   `json:"config"` // file it was read from
}

// SkillGroup is every installed copy of one skill name, across agents.
type SkillGroup struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Installs    []Skill `json:"installs"`
	// Versions counts distinct contents. More than one means the copies differ.
	Versions int `json:"versions"`
}

// Targets returns the distinct "transport target" pairs, sorted.
func (g MCPGroup) Targets() []string {
	var out []string
	for _, s := range g.Servers {
		out = append(out, s.Transport+" "+s.Target)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Agents returns the agents that have this skill, sorted.
func (g SkillGroup) Agents() []string {
	var out []string
	for _, s := range g.Installs {
		out = append(out, s.Agent)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// GroupSkills groups installs by skill name, sorted by name.
func GroupSkills(skills []Skill) []SkillGroup {
	by := map[string]*SkillGroup{}
	for _, s := range skills {
		g := by[s.Name]
		if g == nil {
			g = &SkillGroup{Name: s.Name}
			by[s.Name] = g
		}
		if g.Description == "" {
			g.Description = s.Description
		}
		g.Installs = append(g.Installs, s)
	}
	out := make([]SkillGroup, 0, len(by))
	for _, g := range by {
		hashes := map[string]bool{}
		for _, s := range g.Installs {
			if s.Hash != "" { // "" = not fingerprinted: unknown, not a version
				hashes[s.Hash] = true
			}
		}
		g.Versions = len(hashes)
		slices.SortFunc(g.Installs, func(a, b Skill) int {
			return strings.Compare(a.Agent+a.Scope+a.Path, b.Agent+b.Scope+b.Path)
		})
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b SkillGroup) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// MCPGroup is every configuration of one MCP server name, across agents.
type MCPGroup struct {
	Name    string      `json:"name"`
	Servers []MCPServer `json:"servers"`
	// Differs is set when agents point the same name at different targets.
	Differs bool `json:"differs,omitempty"`
}

// GroupMCP groups servers by name, sorted by name.
func GroupMCP(servers []MCPServer) []MCPGroup {
	by := map[string]*MCPGroup{}
	for _, s := range servers {
		g := by[s.Name]
		if g == nil {
			g = &MCPGroup{Name: s.Name}
			by[s.Name] = g
		}
		g.Servers = append(g.Servers, s)
	}
	out := make([]MCPGroup, 0, len(by))
	for _, g := range by {
		for _, s := range g.Servers[1:] {
			if s.Transport != g.Servers[0].Transport || s.Target != g.Servers[0].Target {
				g.Differs = true
			}
		}
		slices.SortFunc(g.Servers, func(a, b MCPServer) int {
			return strings.Compare(a.Agent+a.Scope+a.Project, b.Agent+b.Scope+b.Project)
		})
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b MCPGroup) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// shortHome replaces the home directory prefix with "~" (paths in errors).
func shortHome(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}
