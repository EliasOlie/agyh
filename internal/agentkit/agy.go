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

func (a *Agy) readMCP() (map[string]json.RawMessage, string, error) {
	path := filepath.Join(a.config, "mcp_config.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, path, fmt.Errorf("%s: %w", shortHome(path), err)
	}
	return doc.MCPServers, path, nil
}

func (a *Agy) MCPConfig() ([]MCPEntry, []error) {
	servers, _, err := a.readMCP()
	if err != nil {
		return nil, errList(err)
	}
	var out []MCPEntry
	for name, raw := range servers {
		out = append(out, MCPEntry{Agent: "agy", Scope: ScopeUser, Name: name, Format: "json", Raw: raw})
	}
	return out, nil
}

// Plugins reads ~/.gemini/config/import_manifest.json ({"imports": [{name, source, …}]}).
func (a *Agy) Plugins() ([]PluginRef, []error) {
	path := filepath.Join(a.config, "import_manifest.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errList(err)
	}
	var doc struct {
		Imports []struct {
			Name   string          `json:"name"`
			Source json.RawMessage `json:"source"`
		} `json:"imports"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errList(fmt.Errorf("%s: %w", shortHome(path), err))
	}
	var out []PluginRef
	for _, imp := range doc.Imports {
		if imp.Name == "" {
			continue
		}
		ref := PluginRef{Agent: "agy", ID: imp.Name}
		var src string
		if json.Unmarshal(imp.Source, &src) == nil {
			ref.Source = src
		} else if len(imp.Source) > 0 && string(imp.Source) != "null" {
			ref.Source = string(imp.Source)
		}
		out = append(out, ref)
	}
	return out, nil
}

func (a *Agy) MCPServers() ([]MCPServer, []error) {
	servers, path, err := a.readMCP()
	if err != nil {
		return nil, errList(err)
	}
	var (
		out  []MCPServer
		errs []error
	)
	for name, entry := range servers {
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
