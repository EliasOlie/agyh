package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/EliasOlie/agyh/internal/model"
)

// ClaudeProjectDir is the folder name Claude Code gives a working directory
// under ~/.claude/projects: every character other than a letter or a digit
// becomes "-" (/home/me/work/_app → -home-me-work--app).
func ClaudeProjectDir(cwd string) string {
	return nonAlnum.ReplaceAllString(cwd, "-")
}

var (
	nonAlnum     = regexp.MustCompile(`[^A-Za-z0-9]`)
	codexRollout = regexp.MustCompile(`^rollout-[A-Za-z0-9._:-]+\.jsonl$`)
)

// TranscriptPath is where an agent looks for a session's file when it is
// started in dir: Claude Code by working directory, Codex by date,
// Antigravity by conversation id (conversations/<id>.db, its own SQLite
// database: agy rebuilds its summary index from it when the session opens).
func TranscriptPath(s model.Session, dir, name string) (string, error) {
	home, _ := os.UserHomeDir()
	switch s.Agent {
	case "claude":
		return filepath.Join(claudeRoot(home), ClaudeProjectDir(dir), s.ID+".jsonl"), nil
	case "agy":
		return filepath.Join(agyRoot(home), "conversations", s.ID+".db"), nil
	case "codex":
		if !codexRollout.MatchString(name) || !strings.Contains(name, s.ID) {
			name = "rollout-" + s.StartedAt.UTC().Format("2006-01-02T15-04-05") + "-" + s.ID + ".jsonl"
		}
		day := s.StartedAt
		if day.IsZero() {
			day = time.Now()
		}
		return filepath.Join(codexRoot(home), day.Format("2006"), day.Format("01"), day.Format("02"), name), nil
	}
	return "", fmt.Errorf("%s does not keep one file per session", s.Agent)
}

// TranscriptSource is this machine's file for a session, the one that lets
// its agent reopen it ("" if there is none here). For Antigravity it is the
// conversation's database, not the transcript agyh reads its text from.
func TranscriptSource(s model.Session) string {
	p := s.Path
	if s.Agent == "agy" {
		p, _ = TranscriptPath(s, "", "")
	}
	if p == "" || strings.HasPrefix(p, "archive:") {
		return ""
	}
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}

// PlaceTranscript writes a session file restored from the vault where the
// agent will find it. An existing file is never replaced: if the agent
// already has this session, that copy wins.
func PlaceTranscript(s model.Session, dir, name string, data []byte) (string, error) {
	p, err := TranscriptPath(s, dir, name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return p, nil
	}
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(p)
		return "", err
	}
	return p, f.Close()
}
