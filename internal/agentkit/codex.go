package agentkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Codex reads OpenAI Codex's skills and MCP servers.
//
// UNTESTED against a real Codex install: this follows OpenAI's public docs
// (learn.chatgpt.com/docs/build-skills, /docs/extend/mcp). If it misreads your
// setup, please open an issue, or better, a PR with a fixture.
//
//   - User skills: $HOME/.agents/skills/<name>/SKILL.md. Older versions and
//     many guides use $CODEX_HOME/skills, which is read too.
//   - Machine-wide skills: /etc/codex/skills.
//   - Disabled skills: [[skills.config]] entries in config.toml with
//     enabled = false, matched by path (to SKILL.md) or name.
//   - MCP servers: [mcp_servers.<name>] in $CODEX_HOME/config.toml
//     (default ~/.codex) with command/args/env or url/http_headers, enabled.
type Codex struct {
	home, agentsSkills, adminSkills string
}

func NewCodex(home string) *Codex {
	return &Codex{
		home:         envOr("CODEX_HOME", filepath.Join(home, ".codex")),
		agentsSkills: filepath.Join(home, ".agents", "skills"),
		adminSkills:  envOr("AGYH_CODEX_ADMIN_SKILLS", "/etc/codex/skills"),
	}
}

func (c *Codex) Name() string    { return "codex" }
func (c *Codex) Available() bool { return exists(c.home) }

type codexConfig struct {
	Skills struct {
		Config []struct {
			Path    string `toml:"path"`
			Name    string `toml:"name"`
			Enabled *bool  `toml:"enabled"`
		} `toml:"config"`
	} `toml:"skills"`
	MCPServers map[string]struct {
		Command           string            `toml:"command"`
		URL               string            `toml:"url"`
		Env               map[string]any    `toml:"env"`
		EnvVars           []any             `toml:"env_vars"`
		BearerTokenEnvVar string            `toml:"bearer_token_env_var"`
		EnvHTTPHeaders    map[string]string `toml:"env_http_headers"`
		Enabled           *bool             `toml:"enabled"`
	} `toml:"mcp_servers"`
}

func (c *Codex) config() (codexConfig, string, error) {
	path := filepath.Join(c.home, "config.toml")
	var cfg codexConfig
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, path, nil
	}
	if err != nil {
		return cfg, path, err
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return cfg, path, fmt.Errorf("%s: %w", shortHome(path), err)
	}
	return cfg, path, nil
}

func (c *Codex) Skills() ([]Skill, []error) {
	if !c.Available() {
		return nil, nil
	}
	var (
		out  []Skill
		errs []error
	)
	for _, d := range []struct{ dir, scope string }{
		{c.agentsSkills, ScopeUser},
		{filepath.Join(c.home, "skills"), ScopeUser},
		{c.adminSkills, ScopeAdmin},
	} {
		s, es := scanSkills(d.dir, "codex", d.scope, "")
		out, errs = append(out, s...), append(errs, es...)
	}
	cfg, _, err := c.config()
	if err != nil {
		return out, append(errs, err)
	}
	// Last matching entry wins (documented).
	for i := range out {
		for _, rule := range cfg.Skills.Config {
			if rule.Enabled == nil {
				continue
			}
			md := filepath.Join(out[i].Path, "SKILL.md")
			if (rule.Path != "" && filepath.Clean(rule.Path) == md) || (rule.Name != "" && rule.Name == out[i].Name) {
				out[i].Disabled = !*rule.Enabled
			}
		}
	}
	return out, errs
}

func (c *Codex) MCPServers() ([]MCPServer, []error) {
	cfg, path, err := c.config()
	if err != nil {
		return nil, []error{err}
	}
	var out []MCPServer
	for name, e := range cfg.MCPServers {
		s := MCPServer{Name: name, Agent: "codex", Scope: ScopeUser, Config: path,
			Disabled: e.Enabled != nil && !*e.Enabled}
		env := sortedKeys(e.Env)
		for _, v := range e.EnvVars {
			switch t := v.(type) {
			case string:
				env = append(env, t)
			case map[string]any:
				if n, ok := t["name"].(string); ok {
					env = append(env, n)
				}
			}
		}
		if e.BearerTokenEnvVar != "" {
			env = append(env, e.BearerTokenEnvVar)
		}
		for _, v := range e.EnvHTTPHeaders {
			env = append(env, v)
		}
		s.EnvNames = sortedKeys(setOf(env))
		switch {
		case e.Command != "":
			s.Transport, s.Target = "stdio", commandName(e.Command)
		case e.URL != "":
			s.Transport, s.Target = "http", urlHost(e.URL)
		default:
			s.Transport = "unknown"
		}
		out = append(out, s)
	}
	return out, nil
}

// MCPConfig converts each [mcp_servers.<name>] table to JSON, values included.
func (c *Codex) MCPConfig() ([]MCPEntry, []error) {
	path := filepath.Join(c.home, "config.toml")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errList(err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, errList(fmt.Errorf("%s: %w", shortHome(path), err))
	}
	var (
		out  []MCPEntry
		errs []error
	)
	for name, table := range doc.MCPServers {
		j, err := json.Marshal(table)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: mcp server %q: %w", shortHome(path), name, err))
			continue
		}
		out = append(out, MCPEntry{Agent: "codex", Scope: ScopeUser, Name: name, Format: "toml", Raw: j})
	}
	return out, errs
}

// Plugins: Codex's on-disk plugin registry is not documented; none are listed.
func (c *Codex) Plugins() ([]PluginRef, []error) { return nil, nil }

func setOf(vals []string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}
