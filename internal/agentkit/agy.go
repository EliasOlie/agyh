package agentkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Agy reads Antigravity CLI's skills and MCP servers. Layout observed on a
// real install: plugins in ~/.gemini/config/plugins/<plugin>/skills/<name>/,
// skills bundled with the CLI in ~/.gemini/antigravity-cli/builtin/skills/,
// MCP servers in ~/.gemini/config/mcp_config.json ("mcpServers").
type Agy struct {
	config, cli string
}

func NewAgy(home string) *Agy {
	return &Agy{
		config: envOr("AGYH_AGY_CONFIG_DIR", filepath.Join(home, ".gemini", "config")),
		cli:    envOr("AGYH_AGY_DIR", filepath.Join(home, ".gemini", "antigravity-cli")),
	}
}

func (a *Agy) Name() string    { return "agy" }
func (a *Agy) Available() bool { return exists(a.config) || exists(a.cli) }

func (a *Agy) Skills() ([]Skill, []error) {
	var (
		out  []Skill
		errs []error
	)
	plugins := filepath.Join(a.config, "plugins")
	entries, err := os.ReadDir(plugins)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		s, es := pluginSkills(filepath.Join(plugins, e.Name()), "agy", e.Name())
		out, errs = append(out, s...), append(errs, es...)
	}
	s, es := scanSkills(filepath.Join(a.cli, "builtin", "skills"), "agy", ScopeBuiltin, "")
	return append(out, s...), append(errs, es...)
}

func (a *Agy) MCPServers() ([]MCPServer, []error) {
	path := filepath.Join(a.config, "mcp_config.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, []error{fmt.Errorf("%s: %w", shortHome(path), err)}
	}
	var (
		out  []MCPServer
		errs []error
	)
	for name, entry := range doc.MCPServers {
		s, err := jsonMCPServer(name, entry)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: mcp server %q: %w", shortHome(path), name, err))
			continue
		}
		s.Agent, s.Scope, s.Config = "agy", ScopeUser, path
		out = append(out, s)
	}
	return out, errs
}
