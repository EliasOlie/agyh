// Package source reads conversations from each coding agent's local storage.
//
// Every agent stores history differently (SQLite + JSONL for Antigravity, one
// JSONL per session for Claude Code and Codex). A Source hides that behind two
// calls: Refs lists sessions cheaply (path + mtime) so the indexer can skip
// unchanged files, and Load parses one session in full.
package source

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/EliasOlie/agyh/internal/model"
)

// Ref points at a session without parsing it.
type Ref struct {
	Agent   string
	ID      string
	Path    string
	ModTime time.Time
	// Meta is optional metadata already known at listing time (e.g. from a DB).
	Meta *model.Session
}

// Source is one agent's history.
type Source interface {
	Name() string
	// Available reports whether this agent's data exists on this machine.
	Available() bool
	Refs(ctx context.Context) ([]Ref, error)
	Load(ctx context.Context, ref Ref) (model.Session, []model.Message, error)
	// Resume returns the working directory and argv that reopen the session.
	Resume(s model.Session) (dir string, argv []string)
}

// All returns every supported source, rooted at the user's home directory.
// Each root can be overridden with an environment variable (see README).
func All() []Source {
	home, _ := os.UserHomeDir()
	return []Source{
		NewAgy(envOr("AGYH_AGY_DIR", filepath.Join(home, ".gemini", "antigravity-cli"))),
		NewClaude(claudeRoot(home)),
		NewCodex(codexRoot(home)),
	}
}

// ByName returns the named source from All, or nil.
func ByName(name string) Source {
	for _, s := range All() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func claudeRoot(home string) string {
	if v := os.Getenv("AGYH_CLAUDE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return filepath.Join(v, "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

func codexRoot(home string) string {
	if v := os.Getenv("AGYH_CODEX_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return filepath.Join(v, "sessions")
	}
	return filepath.Join(home, ".codex", "sessions")
}

// eachJSONLine calls fn for every JSON object line in path. Lines can be very
// long (tool outputs), so it avoids bufio.Scanner's token limit. Malformed
// lines are skipped: agents append while running and the last line may be partial.
func eachJSONLine(ctx context.Context, path string, fn func(raw []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for n := 0; ; n++ {
		if n%512 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[0] == '{' && json.Valid(line) {
			if ferr := fn(line); ferr != nil {
				return ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func parseTime(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t
		}
	}
	return time.Time{}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
