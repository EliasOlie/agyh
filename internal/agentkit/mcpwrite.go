package agentkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// MCPSpec is an MCP server in agent-neutral form: what agyh can carry from one
// agent's format to another's. It holds secrets: never print it.
type MCPSpec struct {
	Transport string            `json:"transport"` // stdio, http or sse
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// Digest fingerprints a spec, to tell agyh's own entries from edited ones.
func (s MCPSpec) Digest() string {
	raw, _ := json.Marshal(s) // map keys are sorted: stable
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// For adapts a spec to what an agent can express: only Claude Code knows
// "sse"; the others get the same URL as http.
func (s MCPSpec) For(agent string) MCPSpec {
	if agent != "claude" && s.Transport == "sse" {
		s.Transport = "http"
	}
	return s
}

// Summary describes a spec without secrets ("stdio gh-mcp", "http https://host").
func (s MCPSpec) Summary() string {
	if s.Transport == "stdio" {
		return "stdio " + commandName(s.Command)
	}
	return s.Transport + " " + urlHost(s.URL)
}

// SpecFromEntry converts a stored entry to a spec. Fields that have no
// equivalent in the other agents are reported in warnings, never silently dropped.
func SpecFromEntry(e MCPEntry) (MCPSpec, []string, error) {
	var raw map[string]any
	if err := json.Unmarshal(e.Raw, &raw); err != nil {
		return MCPSpec{}, nil, err
	}
	str := func(k string) string { v, _ := raw[k].(string); return v }
	strMap := func(k string) map[string]string {
		m, _ := raw[k].(map[string]any)
		if len(m) == 0 {
			return nil
		}
		out := map[string]string{}
		for key, v := range m {
			out[key] = fmt.Sprint(v)
		}
		return out
	}
	var (
		s        MCPSpec
		warnings []string
		used     = map[string]bool{}
	)
	use := func(keys ...string) {
		for _, k := range keys {
			used[k] = true
		}
	}
	if args, ok := raw["args"].([]any); ok {
		for _, a := range args {
			s.Args = append(s.Args, fmt.Sprint(a))
		}
	}
	s.Env = strMap("env")
	use("command", "args", "env", "type", "disabled", "enabled")
	switch {
	case str("command") != "":
		s.Transport, s.Command = "stdio", str("command")
	case e.Format == "toml" && str("url") != "":
		s.Transport, s.URL = "http", str("url")
		s.Headers = strMap("http_headers")
		use("url", "http_headers")
		for header, envVar := range strMap("env_http_headers") {
			if s.Headers == nil {
				s.Headers = map[string]string{}
			}
			s.Headers[header] = "${" + envVar + "}"
		}
		use("env_http_headers")
		if v := str("bearer_token_env_var"); v != "" {
			if s.Headers == nil {
				s.Headers = map[string]string{}
			}
			s.Headers["Authorization"] = "Bearer ${" + v + "}"
			warnings = append(warnings, "bearer_token_env_var became an Authorization header with ${"+v+"}")
		}
		use("bearer_token_env_var")
	case str("url") != "" || str("serverUrl") != "":
		s.Transport = remoteTransport(str("type"))
		s.URL = firstNonEmpty(str("url"), str("serverUrl"))
		s.Headers = strMap("headers")
		use("url", "serverUrl", "headers")
	default:
		return MCPSpec{}, nil, errors.New("neither a command nor a url")
	}
	var dropped []string
	for k := range raw {
		if !used[k] {
			dropped = append(dropped, k)
		}
	}
	if len(dropped) > 0 {
		slices.Sort(dropped)
		warnings = append(warnings, "not carried over: "+strings.Join(dropped, ", "))
	}
	return s, warnings, nil
}

// MCPTarget is an agent agyh can write MCP servers into (user scope only).
type MCPTarget interface {
	Home
	// WriteMCP adds the server, or replaces it when replace is set.
	WriteMCP(name string, s MCPSpec, replace bool) error
	RemoveMCP(name string) error
}

// MCPTargetOf returns h as an MCPTarget.
func MCPTargetOf(h Home) (MCPTarget, error) {
	if t, ok := h.(MCPTarget); ok {
		return t, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotATarget, h.Name())
}

// RunCLI runs an agent's own CLI (replaceable in tests).
var RunCLI = func(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// --- Claude Code: through its CLI. ~/.claude.json is rewritten by Claude
// Code all the time; editing it from outside could lose its writes.

func (c *Claude) WriteMCP(name string, s MCPSpec, replace bool) error {
	if replace {
		if err := c.RemoveMCP(name); err != nil {
			return err
		}
	}
	entry := map[string]any{}
	if s.Transport == "stdio" {
		entry["type"], entry["command"] = "stdio", s.Command
		if len(s.Args) > 0 {
			entry["args"] = s.Args
		}
		if len(s.Env) > 0 {
			entry["env"] = s.Env
		}
	} else {
		entry["type"], entry["url"] = s.Transport, s.URL
		if len(s.Headers) > 0 {
			entry["headers"] = s.Headers
		}
	}
	raw, _ := json.Marshal(entry)
	if out, err := RunCLI("claude", "mcp", "add-json", "--scope", "user", name, string(raw)); err != nil {
		return fmt.Errorf("claude mcp add-json: %w: %s", err, redactCLI(out, raw))
	}
	return nil
}

func (c *Claude) RemoveMCP(name string) error {
	if out, err := RunCLI("claude", "mcp", "remove", "--scope", "user", name); err != nil {
		return fmt.Errorf("claude mcp remove: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// redactCLI keeps a CLI error readable without echoing the secret-bearing JSON.
func redactCLI(out, secret []byte) string {
	return strings.TrimSpace(string(bytes.ReplaceAll(out, secret, []byte("<config>"))))
}

// --- Antigravity: mcp_config.json, edited in place. Every other key and
// server keeps its content (the file is re-indented); the previous file is
// kept as a backup next to it.

func (a *Agy) WriteMCP(name string, s MCPSpec, _ bool) error {
	entry := map[string]any{}
	if s.Transport == "stdio" {
		entry["command"] = s.Command
		if len(s.Args) > 0 {
			entry["args"] = s.Args
		}
		if len(s.Env) > 0 {
			entry["env"] = s.Env
		}
	} else {
		entry["serverUrl"] = s.URL
		if len(s.Headers) > 0 {
			entry["headers"] = s.Headers
		}
	}
	raw, _ := json.Marshal(entry)
	return a.editMCP(func(servers map[string]json.RawMessage) { servers[name] = raw })
}

func (a *Agy) RemoveMCP(name string) error {
	return a.editMCP(func(servers map[string]json.RawMessage) { delete(servers, name) })
}

func (a *Agy) editMCP(change func(map[string]json.RawMessage)) error {
	path := filepath.Join(a.config, "mcp_config.json")
	orig, err := os.ReadFile(path)
	mode := os.FileMode(0o600)
	doc := map[string]json.RawMessage{}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		orig = nil
	case err != nil:
		return err
	default:
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
		if err := json.Unmarshal(orig, &doc); err != nil {
			return fmt.Errorf("%s: %w", shortHome(path), err)
		}
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return fmt.Errorf("%s: mcpServers: %w", shortHome(path), err)
		}
	}
	change(servers)
	doc["mcpServers"], _ = json.Marshal(servers)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if orig != nil {
		if err := os.WriteFile(path+".agyh-backup", orig, 0o600); err != nil {
			return err
		}
	}
	return writeAtomic(path, append(out, '\n'), mode)
}

// --- Codex: config.toml is hand-edited and commented, and re-encoding it
// would lose that. agyh appends one marked block per server and only ever
// removes its own block.

func codexMarkers(name string) (string, string) {
	return "# agyh:begin mcp " + name + " (managed by agyh; edit freely, agyh then leaves it alone)", "# agyh:end mcp " + name
}

func (c *Codex) WriteMCP(name string, s MCPSpec, replace bool) error {
	if replace {
		if err := c.RemoveMCP(name); err != nil {
			return err
		}
	}
	table := map[string]any{}
	if s.Transport == "stdio" {
		table["command"] = s.Command
		if len(s.Args) > 0 {
			table["args"] = s.Args
		}
		if len(s.Env) > 0 {
			table["env"] = s.Env
		}
	} else {
		table["url"] = s.URL
		if len(s.Headers) > 0 {
			table["http_headers"] = s.Headers
		}
	}
	var body bytes.Buffer
	if err := toml.NewEncoder(&body).Encode(map[string]any{"mcp_servers": map[string]any{name: table}}); err != nil {
		return err
	}
	path := filepath.Join(c.home, "config.toml")
	orig, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	begin, end := codexMarkers(name)
	var out bytes.Buffer
	out.Write(orig)
	if len(orig) > 0 && !bytes.HasSuffix(orig, []byte("\n")) {
		out.WriteByte('\n')
	}
	fmt.Fprintf(&out, "\n%s\n%s%s\n", begin, strings.TrimLeft(body.String(), "\n"), end)
	// The result must still parse, or nothing is written.
	var check map[string]any
	if _, err := toml.Decode(out.String(), &check); err != nil {
		return fmt.Errorf("refusing to write an invalid config.toml: %w", err)
	}
	if err := os.MkdirAll(c.home, 0o700); err != nil {
		return err
	}
	if orig != nil {
		if err := os.WriteFile(path+".agyh-backup", orig, 0o600); err != nil {
			return err
		}
	}
	return writeAtomic(path, out.Bytes(), 0o600)
}

func (c *Codex) RemoveMCP(name string) error {
	path := filepath.Join(c.home, "config.toml")
	orig, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	begin, end := codexMarkers(name)
	text := string(orig)
	i := strings.Index(text, "\n"+begin+"\n")
	j := strings.Index(text, "\n"+end+"\n")
	if i < 0 || j < i {
		return fmt.Errorf("agyh's block for %q is not in %s: left untouched", name, shortHome(path))
	}
	updated := text[:i] + text[j+len("\n"+end+"\n"):]
	if err := os.WriteFile(path+".agyh-backup", orig, 0o600); err != nil {
		return err
	}
	return writeAtomic(path, []byte(updated), 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".agyh-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- Planning, with the same rules as skills (see link.go).

// MCPLink is one MCP server agyh wrote into an agent.
type MCPLink struct {
	Agent       string    `json:"agent"`
	Name        string    `json:"name"`
	Digest      string    `json:"digest"`
	Source      string    `json:"source"`
	InstalledAt time.Time `json:"installed_at"`
}

// MCPStep is one planned MCP change (Kind is a Step* constant).
type MCPStep struct {
	Kind    string `json:"kind"`
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Source  string `json:"source,omitempty"`
	Summary string `json:"summary"`
	spec    MCPSpec
	target  MCPTarget
}

func (s MCPStep) Refused() bool { return s.Kind == StepUnmanaged || s.Kind == StepModified }

func (l *Ledger) findMCP(agent, name string) (MCPLink, bool) {
	for _, k := range l.MCP {
		if k.Agent == agent && k.Name == name {
			return k, true
		}
	}
	return MCPLink{}, false
}

// currentSpec returns the user-scope server called name in target, if any.
func currentSpec(target Home, name string) (MCPSpec, bool, error) {
	entries, errs := target.MCPConfig()
	if err := errors.Join(errs...); err != nil {
		return MCPSpec{}, false, err
	}
	for _, e := range entries {
		if e.Name == name && e.Scope == ScopeUser {
			s, _, err := SpecFromEntry(e)
			return s, true, err
		}
	}
	return MCPSpec{}, false, nil
}

// PlanMCP decides what writing spec as name into target would do.
func (l *Ledger) PlanMCP(name string, spec MCPSpec, target MCPTarget, source string) (MCPStep, error) {
	if !safeDirName.MatchString(name) {
		return MCPStep{}, fmt.Errorf("server name %q is not safe to write", name)
	}
	spec = spec.For(target.Name())
	st := MCPStep{Agent: target.Name(), Name: name, Source: source, Summary: spec.Summary(), spec: spec, target: target}
	cur, exists, err := currentSpec(target, name)
	if err != nil {
		return MCPStep{}, err
	}
	link, managed := l.findMCP(target.Name(), name)
	switch {
	case !exists:
		st.Kind = StepInstall
	case !managed && cur.Digest() == spec.Digest():
		st.Kind = StepPresent
	case !managed:
		st.Kind = StepUnmanaged
	case cur.Digest() != link.Digest:
		st.Kind = StepModified
	case cur.Digest() == spec.Digest():
		st.Kind = StepUpToDate
	default:
		st.Kind = StepUpdate
	}
	return st, nil
}

// PlanMCPRemove decides what removing agyh's server would do.
func (l *Ledger) PlanMCPRemove(link MCPLink, target MCPTarget) (MCPStep, error) {
	st := MCPStep{Agent: link.Agent, Name: link.Name, Source: link.Source, target: target}
	cur, exists, err := currentSpec(target, link.Name)
	switch {
	case err != nil:
		return st, err
	case !exists:
		st.Kind = StepForget
	case cur.Digest() != link.Digest:
		st.Kind = StepModified
	default:
		st.Kind, st.Summary = StepRemove, cur.Summary()
	}
	return st, nil
}

// ApplyMCP performs a planned step, re-planning first so a server changed
// in between is left alone.
func (l *Ledger) ApplyMCP(st MCPStep) error {
	switch st.Kind {
	case StepInstall, StepUpdate:
		again, err := l.PlanMCP(st.Name, st.spec, st.target, st.Source)
		if err != nil {
			return err
		}
		if again.Kind != st.Kind {
			return fmt.Errorf("%s changed in %s meanwhile: left untouched", st.Name, st.Agent)
		}
		if err := st.target.WriteMCP(st.Name, st.spec, st.Kind == StepUpdate); err != nil {
			return err
		}
		// Record what the agent actually stored (it may normalize the entry),
		// so a later plan doesn't mistake its own write for a user edit.
		written, ok, err := currentSpec(st.target, st.Name)
		if err != nil || !ok {
			return fmt.Errorf("%s: wrote %s but cannot read it back: %v", st.Agent, st.Name, err)
		}
		l.MCP = slices.DeleteFunc(l.MCP, func(k MCPLink) bool { return k.Agent == st.Agent && k.Name == st.Name })
		l.MCP = append(l.MCP, MCPLink{Agent: st.Agent, Name: st.Name, Digest: written.Digest(), Source: st.Source, InstalledAt: time.Now().UTC()})
		return l.Save()
	case StepRemove:
		link, ok := l.findMCP(st.Agent, st.Name)
		if !ok {
			return fmt.Errorf("%s is not managed by agyh", st.Name)
		}
		again, err := l.PlanMCPRemove(link, st.target)
		if err != nil {
			return err
		}
		if again.Kind != StepRemove {
			return fmt.Errorf("%s changed in %s meanwhile: left untouched", st.Name, st.Agent)
		}
		if err := st.target.RemoveMCP(st.Name); err != nil {
			return err
		}
		fallthrough
	case StepForget:
		l.MCP = slices.DeleteFunc(l.MCP, func(k MCPLink) bool { return k.Agent == st.Agent && k.Name == st.Name })
		return l.Save()
	}
	return nil
}
