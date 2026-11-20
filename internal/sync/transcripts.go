package sync

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/EliasOlie/agyh/internal/model"
)

// Transcripts: a session bundle holds the conversation's text, enough to
// search and read it anywhere. Reopening it in its agent needs the agent's
// own file, tool calls and all, so for agents that keep one file per
// session (Claude Code, Codex) the vault also stores that file, gzipped and
// encrypted like everything else:
//
//	<prefix>/transcripts/<agent>/<id>.blob
//
// SessionSummary.Transcript records the file's name; `agyh resume` puts it
// back where the agent looks (see source.PlaceTranscript).

// transcriptAgents keep one transcript file per session.
var transcriptAgents = []string{"claude", "codex"}

// maxTranscript caps one transcript (uncompressed).
const maxTranscript = 256 << 20

// ErrNoTranscript: the vault has the conversation's text but not its file.
var ErrNoTranscript = errors.New("the vault has no transcript of this session")

func (e *Engine) transcriptKey(agent, id string) string {
	return path.Join(e.cfg.Prefix, "transcripts", agent, id+".blob")
}

// hasTranscriptFile reports whether this machine has the agent's own file
// for the session (not one restored from the vault's text).
func hasTranscriptFile(s model.Session) bool {
	if !slices.Contains(transcriptAgents, s.Agent) || s.Path == "" || strings.HasPrefix(s.Path, "archive:") {
		return false
	}
	fi, err := os.Stat(s.Path)
	return err == nil && fi.Mode().IsRegular()
}

// uploadTranscript stores the session's file and returns its name ("" when
// there is nothing to store: transcripts off, another agent, no local file).
func (e *Engine) uploadTranscript(ctx context.Context, s model.Session) (string, error) {
	if !e.cfg.TranscriptsEnabled() || !hasTranscriptFile(s) {
		return "", nil
	}
	f, err := os.Open(s.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	n, err := io.Copy(gz, io.LimitReader(f, maxTranscript+1))
	if err != nil {
		return "", fmt.Errorf("read transcript: %w", err)
	}
	if n > maxTranscript {
		return "", nil // too big to keep: the text is still in the bundle
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	keys, err := e.ready(ctx, true)
	if err != nil {
		return "", err
	}
	sealed, err := keys.seal(buf.Bytes())
	if err != nil {
		return "", err
	}
	if err := e.s3.Put(ctx, e.transcriptKey(s.Agent, s.ID), sealed); err != nil {
		return "", fmt.Errorf("upload transcript: %w", err)
	}
	return filepath.Base(s.Path), nil
}

// backfillTranscripts stores the files of sessions already in the vault
// without one (backed up by an older agyh), when this machine has them.
func (e *Engine) backfillTranscripts(ctx context.Context, m *Manifest, l *localState, skip map[string]bool) (int, error) {
	if !e.cfg.TranscriptsEnabled() {
		return 0, nil
	}
	var todo []model.Session
	for _, s := range l.sessions {
		rem, ok := m.Sessions[s.Key()]
		if ok && rem.Transcript == "" && !skip[s.Key()] && hasTranscriptFile(s) {
			todo = append(todo, s)
		}
	}
	names := make([]string, len(todo))
	err := each(ctx, len(todo), func(ctx context.Context, i int) error {
		name, err := e.uploadTranscript(ctx, todo[i])
		if err != nil {
			return fmt.Errorf("transcript %s: %w", todo[i].Key(), err)
		}
		names[i] = name
		return nil
	})
	n := 0
	for i, s := range todo {
		if names[i] == "" {
			continue
		}
		rem := m.Sessions[s.Key()]
		rem.Transcript = names[i]
		m.Sessions[s.Key()] = rem
		n++
	}
	return n, err
}

// FetchTranscript downloads a session's original file from the vault.
func (e *Engine) FetchTranscript(ctx context.Context, agent, id string) (name string, data []byte, err error) {
	m, err := e.fetchManifest(ctx)
	if err != nil {
		return "", nil, err
	}
	rem, ok := m.Sessions[agent+":"+id]
	if !ok || rem.Transcript == "" {
		return "", nil, ErrNoTranscript
	}
	_, packed, err := e.getOpen(ctx, e.transcriptKey(agent, id))
	if errors.Is(err, ErrObjectNotFound) {
		return "", nil, ErrNoTranscript
	}
	if err != nil {
		return "", nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return "", nil, fmt.Errorf("transcript: %w", err)
	}
	data, err = io.ReadAll(io.LimitReader(gz, maxTranscript+1))
	if err != nil {
		return "", nil, fmt.Errorf("transcript: %w", err)
	}
	return filepath.Base(rem.Transcript), data, nil
}
