// Package hookinstall writes agyh's hooks into each agent's settings, and
// finds hooks that run a program that no longer exists (an agent then fails
// every tool call, with no hint that agyh is not the culprit).
//
// It only ever replaces hooks that call "agyh hook" and, when asked, removes
// broken ones; every other setting is kept, and the previous file is saved
// next to it with an ".agyh-backup" suffix.
package hookinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Agents are the agents agyh can install hooks for.
var Agents = []string{"claude", "agy", "codex"}

// Paths locates each agent's hook files.
type Paths struct {
	Home string
	// Claude is Claude Code's settings.json ($CLAUDE_CONFIG_DIR or ~/.claude).
	Claude string
	// Agy is the Antigravity hooks file agyh writes; AgyLegacy is the older
	// location some setups still have (Antigravity reads both).
	Agy, AgyLegacy string
	// Codex is $CODEX_HOME/hooks.json (default ~/.codex).
	Codex string
}

// DefaultPaths resolves the hook files for the current user.
func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	claude := filepath.Join(home, ".claude")
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		claude = v
	}
	codex := filepath.Join(home, ".codex")
	if v := os.Getenv("CODEX_HOME"); v != "" {
		codex = v
	}
	return Paths{
		Home:      home,
		Claude:    filepath.Join(claude, "settings.json"),
		Agy:       filepath.Join(home, ".agents", "hooks.json"),
		AgyLegacy: filepath.Join(home, ".gemini", "config", "hooks.json"),
		Codex:     filepath.Join(codex, "hooks.json"),
	}
}

// File returns the hook file agyh writes for an agent.
func (p Paths) File(agent string) string {
	switch agent {
	case "claude":
		return p.Claude
	case "agy":
		return p.Agy
	case "codex":
		return p.Codex
	}
	return ""
}

// all lists every hook file agyh looks at, for stale-hook checks.
func (p Paths) all() []string {
	return []string{p.Claude, p.Agy, p.AgyLegacy, p.Codex}
}

// Detected reports whether an agent seems installed (its config folder exists).
func (p Paths) Detected(agent string) bool {
	dir := filepath.Dir(p.File(agent))
	if agent == "agy" {
		dir = filepath.Join(p.Home, ".gemini")
	}
	_, err := os.Stat(dir)
	return err == nil
}

func handler(command string, timeout int) map[string]any {
	return map[string]any{"type": "command", "command": command, "timeout": timeout}
}

func group(matcher string, h map[string]any) map[string]any {
	g := map[string]any{"hooks": []any{h}}
	if matcher != "" {
		g["matcher"] = matcher
	}
	return g
}

// Snippet is the hook configuration for an agent, calling the agyh binary at bin.
//
//   - claude, codex: PreToolUse only enforces the local policy (fatal commands
//     blocked even in auto modes); PermissionRequest asks your phone, and only
//     when the agent itself would have asked you; Stop notifies.
//   - agy: one PreToolUse hook that asks your phone and the terminal.
func Snippet(agent, bin string) (map[string]any, error) {
	q := quote(bin)
	switch agent {
	case "claude", "codex":
		return map[string]any{"hooks": map[string]any{
			"PreToolUse":        []any{group("Bash", handler(q+" hook pre-tool-use --agent "+agent+" --policy-only", 10))},
			"PermissionRequest": []any{group("*", handler(q+" hook permission-request --agent "+agent, 180))},
			"Stop":              []any{group("", handler(q+" hook stop --agent "+agent, 15))},
		}}, nil
	case "agy":
		return map[string]any{
			"agyh-permission": map[string]any{"PreToolUse": []any{group("run_command", handler(q+" hook pre-tool-use --agent agy", 180))}},
			"agyh-stop":       map[string]any{"Stop": []any{handler(q+" hook stop --agent agy", 15)}},
		}, nil
	}
	return nil, fmt.Errorf("unsupported agent %q (supported: %s)", agent, strings.Join(Agents, ", "))
}

// quote wraps a path with spaces for the shell that runs hook commands.
func quote(p string) string {
	if strings.ContainsAny(p, " \t'\"") {
		return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
	}
	return p
}

// Stale is a hook whose program is missing.
type Stale struct {
	File, Command, Missing string
}

// Result describes what Install changed.
type Result struct {
	Agent, File, Backup string
	Created             bool
	Removed             []Stale // broken hooks dropped from that file
}

// Install writes agyh's hooks for one agent, replacing earlier agyh hooks
// and dropping broken ones in that file. Other settings are kept.
func Install(p Paths, agent, bin string) (Result, error) {
	snippet, err := Snippet(agent, bin)
	if err != nil {
		return Result{}, err
	}
	path := p.File(agent)
	res := Result{Agent: agent, File: path}
	doc, orig, mode, err := readJSON(path)
	if err != nil {
		return res, err
	}
	res.Created = orig == nil
	isStale := staleChecker(p.Home)

	switch agent {
	case "claude", "codex":
		hooks, _ := doc["hooks"].(map[string]any)
		if hooks == nil {
			hooks = map[string]any{}
		}
		pruned, removed := prune(hooks, func(cmd string) (bool, string) {
			if isAgyhHook(cmd) {
				return true, "" // replaced below
			}
			return isStale(cmd)
		})
		hooks, _ = pruned.(map[string]any)
		if hooks == nil {
			hooks = map[string]any{}
		}
		for event, groups := range snippet["hooks"].(map[string]any) {
			existing, _ := hooks[event].([]any)
			hooks[event] = append(existing, groups.([]any)...)
		}
		doc["hooks"] = hooks
		res.Removed = staleOnly(path, removed)
	case "agy":
		pruned, removed := prune(doc, func(cmd string) (bool, string) {
			if isAgyhHook(cmd) {
				return true, ""
			}
			return isStale(cmd)
		})
		doc, _ = pruned.(map[string]any)
		if doc == nil {
			doc = map[string]any{}
		}
		for name, v := range snippet {
			doc[name] = v
		}
		res.Removed = staleOnly(path, removed)
	}
	res.Backup, err = writeJSON(path, doc, orig, mode)
	return res, err
}

// FindStale lists hooks, in every agent's hook files, that run a missing program.
func FindStale(p Paths) ([]Stale, error) {
	isStale := staleChecker(p.Home)
	var out []Stale
	for _, path := range p.all() {
		doc, _, _, err := readJSON(path)
		if err != nil {
			return out, err
		}
		root := any(doc)
		if path == p.Claude || path == p.Codex {
			root = doc["hooks"]
		}
		walkCommands(root, func(cmd string) {
			if stale, missing := isStale(cmd); stale {
				out = append(out, Stale{File: path, Command: cmd, Missing: missing})
			}
		})
	}
	return out, nil
}

// RemoveStale drops broken hooks from one file (with a backup).
func RemoveStale(p Paths, path string) ([]Stale, string, error) {
	doc, orig, mode, err := readJSON(path)
	if err != nil || orig == nil {
		return nil, "", err
	}
	isStale := staleChecker(p.Home)
	var removed []string
	if path == p.Claude || path == p.Codex {
		hooks, ok := doc["hooks"].(map[string]any)
		if !ok {
			return nil, "", nil
		}
		pruned, r := prune(hooks, isStale)
		removed = r
		if pruned == nil {
			delete(doc, "hooks")
		} else {
			doc["hooks"] = pruned
		}
	} else {
		pruned, r := prune(doc, isStale)
		removed = r
		doc, _ = pruned.(map[string]any)
		if doc == nil {
			doc = map[string]any{}
		}
	}
	if len(removed) == 0 {
		return nil, "", nil
	}
	backup, err := writeJSON(path, doc, orig, mode)
	return staleOnly(path, removed), backup, err
}

func isAgyhHook(cmd string) bool {
	f := strings.Fields(cmd)
	for i := 0; i+1 < len(f); i++ {
		if filepath.Base(strings.Trim(f[i], `'"`)) == "agyh" && f[i+1] == "hook" {
			return true
		}
	}
	return false
}

// staleChecker reports whether a hook command runs a file that is gone. It
// looks at the program and, for interpreters (python3 script.py, bash x.sh,
// node x.js), at the script. Commands found on PATH, or with shell syntax
// it cannot judge, are never reported.
func staleChecker(home string) func(cmd string) (bool, string) {
	return func(cmd string) (bool, string) {
		f := strings.Fields(cmd)
		for len(f) > 0 && strings.Contains(f[0], "=") && !strings.HasPrefix(f[0], "/") {
			f = f[1:] // FOO=bar program …
		}
		if len(f) == 0 || strings.ContainsAny(cmd, "$`|;&<>") {
			return false, ""
		}
		candidates := []string{f[0]}
		switch filepath.Base(f[0]) {
		case "python", "python3", "bash", "sh", "zsh", "node", "ruby", "perl", "deno", "bun":
			for _, a := range f[1:] {
				if !strings.HasPrefix(a, "-") {
					candidates = append(candidates, a)
					break
				}
			}
		}
		for _, c := range candidates {
			c = strings.Trim(c, `'"`)
			if strings.HasPrefix(c, "~/") {
				c = filepath.Join(home, c[2:])
			}
			if !filepath.IsAbs(c) {
				continue // resolved through PATH or the hook's cwd: can't tell
			}
			if _, err := os.Stat(c); errors.Is(err, fs.ErrNotExist) {
				return true, c
			}
		}
		return false, ""
	}
}

// prune removes hook handlers whose command matches drop, then groups and
// entries left without handlers. It returns nil when nothing is left.
func prune(v any, drop func(cmd string) (bool, string)) (any, []string) {
	var removed []string
	var walk func(v any) (any, bool)
	walk = func(v any) (any, bool) { // (new value, keep)
		switch t := v.(type) {
		case map[string]any:
			if cmd, ok := t["command"].(string); ok {
				if d, _ := drop(cmd); d {
					removed = append(removed, cmd)
					return nil, false
				}
				return t, true
			}
			if len(t) == 0 {
				return t, true
			}
			out := map[string]any{}
			changed := false
			for k, child := range t {
				nv, keep := walk(child)
				if keep {
					out[k] = nv
				} else {
					changed = true
				}
			}
			if changed && !hasHandlers(out) {
				return nil, false
			}
			return out, true
		case []any:
			if len(t) == 0 {
				return t, true
			}
			out := []any{}
			for _, child := range t {
				if nv, keep := walk(child); keep {
					out = append(out, nv)
				}
			}
			if len(out) == 0 {
				return nil, false
			}
			return out, true
		}
		return v, true
	}
	nv, keep := walk(v)
	if !keep {
		return nil, removed
	}
	return nv, removed
}

// hasHandlers reports whether something with a "command" is still inside.
func hasHandlers(v any) bool {
	found := false
	walkCommands(v, func(string) { found = true })
	return found
}

func walkCommands(v any, fn func(cmd string)) {
	switch t := v.(type) {
	case map[string]any:
		if cmd, ok := t["command"].(string); ok {
			fn(cmd)
			return
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkCommands(t[k], fn)
		}
	case []any:
		for _, c := range t {
			walkCommands(c, fn)
		}
	}
}

func staleOnly(path string, removed []string) []Stale {
	var out []Stale
	for _, cmd := range removed {
		if isAgyhHook(cmd) {
			continue
		}
		out = append(out, Stale{File: path, Command: cmd})
	}
	return out
}

// readJSON loads a settings file; a missing or empty file is an empty object.
func readJSON(path string) (map[string]any, []byte, fs.FileMode, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil, 0o600, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	doc := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, nil, 0, fmt.Errorf("%s is not valid JSON (left untouched): %w", path, err)
		}
	}
	return doc, b, mode, nil
}

// writeJSON saves doc atomically, keeping the previous content as a backup.
func writeJSON(path string, doc map[string]any, orig []byte, mode fs.FileMode) (string, error) {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	out = append(out, '\n')
	if bytes.Equal(out, orig) {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	backup := ""
	if orig != nil {
		backup = path + ".agyh-backup"
		if err := os.WriteFile(backup, orig, 0o600); err != nil {
			return "", err
		}
	}
	tmp := path + ".agyh-tmp"
	if err := os.WriteFile(tmp, out, mode); err != nil {
		return "", err
	}
	return backup, os.Rename(tmp, path)
}
