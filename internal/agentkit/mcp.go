package agentkit

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
)

// jsonMCPServer reads one server entry in the JSON shape Claude Code and
// Antigravity share: {command, args, env} for stdio, {type, url|serverUrl,
// headers} for remote servers, optional disabled. Only non-secret facts are
// kept: see MCPServer.
func jsonMCPServer(name string, raw json.RawMessage) (MCPServer, error) {
	var e struct {
		Type      string         `json:"type"`
		Command   string         `json:"command"`
		URL       string         `json:"url"`
		ServerURL string         `json:"serverUrl"`
		Env       map[string]any `json:"env"`
		Headers   map[string]any `json:"headers"`
		Disabled  bool           `json:"disabled"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return MCPServer{}, err
	}
	s := MCPServer{Name: name, Disabled: e.Disabled, EnvNames: sortedKeys(e.Env)}
	remote := firstNonEmpty(e.URL, e.ServerURL)
	switch {
	case e.Command != "":
		s.Transport, s.Target = "stdio", commandName(e.Command)
	case remote != "":
		s.Transport, s.Target = remoteTransport(e.Type), urlHost(remote)
	default:
		s.Transport, s.Target = firstNonEmpty(e.Type, "unknown"), ""
	}
	return s, nil
}

func remoteTransport(t string) string {
	switch strings.ToLower(t) {
	case "sse":
		return "sse"
	case "ws":
		return "ws"
	}
	return "http" // "http", "streamable-http" or unset
}

// commandName keeps only the program name: arguments often carry tokens.
func commandName(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(fields[0])
}

// urlHost keeps only scheme and host: paths and queries may embed tokens.
// Unexpanded "${VAR}" templates are reported as such, never resolved.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if strings.Contains(raw, "${") {
			return "(templated url)"
		}
		return "(invalid url)"
	}
	return u.Scheme + "://" + u.Hostname()
}

func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func errList(err error) []error {
	if err == nil {
		return nil
	}
	return []error{err}
}
