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
	"strconv"
	"strings"

	"github.com/EliasOlie/agyh/internal/model"
)

// Transcripts: a session bundle holds the conversation's text, enough to
// search and read it anywhere. Reopening it in its agent needs the agent's
// own file, tool calls and all, so for agents that keep one file per
// session (Claude Code, Codex) the vault also stores that file, gzipped and
// encrypted like everything else:
//
//	<prefix>/transcripts/<agent>/<id>.blob        small enough for one object
//	<prefix>/transcripts/<agent>/<id>.<n>.blob    parts 1..N of a big one
//
// Long sessions reach tens of MB and some S3 servers (or the proxy in front
// of them) refuse objects that large, so big transcripts go in parts. The
// manifest records the file's name, the number of parts and a fingerprint;
// `agyh resume` puts the file back where the agent looks
// (see source.PlaceTranscript).

// transcriptAgents keep one transcript file per session.
var transcriptAgents = []string{"claude", "codex"}

// maxTranscript caps one transcript (uncompressed).
const maxTranscript = 256 << 20

// transcriptPart is the size of one transcript part, before encryption
// (which adds a header and may grow incompressible data slightly). Some
// self-hosted S3 servers refuse objects over 4 MiB, so parts stay at 3 MiB.
var transcriptPart = 3 << 20

// ErrNoTranscript: the vault has the conversation's text but not its file.
var ErrNoTranscript = errors.New("the vault has no transcript of this session")

func (e *Engine) transcriptKey(agent, id string, part int) string {
	name := id + ".blob"
	if part > 0 {
		name = id + "." + strconv.Itoa(part) + ".blob"
	}
	return path.Join(e.cfg.Prefix, "transcripts", agent, name)
}

// transcriptRef is what the manifest records about a stored transcript.
type transcriptRef struct {
	Name  string
	Parts int
	SHA   string
}

func (r transcriptRef) apply(s *SessionSummary) {
	s.Transcript, s.TranscriptParts, s.TranscriptSHA = r.Name, r.Parts, r.SHA
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

// uploadTranscript stores the session's file. The ref is empty when there
// is nothing to store: transcripts off, another agent, no local file.
func (e *Engine) uploadTranscript(ctx context.Context, s model.Session) (transcriptRef, error) {
	if !e.cfg.TranscriptsEnabled() || !hasTranscriptFile(s) {
		return transcriptRef{}, nil
	}
	f, err := os.Open(s.Path)
	if err != nil {
		return transcriptRef{}, err
	}
	defer f.Close()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	n, err := io.Copy(gz, io.LimitReader(f, maxTranscript+1))
	if err != nil {
		return transcriptRef{}, fmt.Errorf("read transcript: %w", err)
	}
	if n > maxTranscript {
		return transcriptRef{}, nil // too big to keep: the text is still in the bundle
	}
	if err := gz.Close(); err != nil {
		return transcriptRef{}, err
	}
	packed := buf.Bytes()
	keys, err := e.ready(ctx, true)
	if err != nil {
		return transcriptRef{}, err
	}
	ref := transcriptRef{Name: filepath.Base(s.Path), Parts: 1, SHA: digestOf(packed)}
	put := func(key string, data []byte) error {
		sealed, err := keys.seal(data)
		if err != nil {
			return err
		}
		if err := e.s3.Put(ctx, key, sealed); err != nil {
			return fmt.Errorf("upload transcript: %w", err)
		}
		return nil
	}
	if len(packed) <= transcriptPart {
		return ref, put(e.transcriptKey(s.Agent, s.ID, 0), packed)
	}
	ref.Parts = (len(packed) + transcriptPart - 1) / transcriptPart
	for i := range ref.Parts {
		end := min((i+1)*transcriptPart, len(packed))
		if err := put(e.transcriptKey(s.Agent, s.ID, i+1), packed[i*transcriptPart:end]); err != nil {
			return transcriptRef{}, fmt.Errorf("part %d of %d: %w", i+1, ref.Parts, err)
		}
	}
	return ref, nil
}

// backfillTranscripts stores the files of sessions already in the vault
// without one (backed up by an older agyh, or refused last time), when this
// machine has them. Failures are warnings: the next sync tries again.
func (e *Engine) backfillTranscripts(ctx context.Context, m *Manifest, l *localState, skip map[string]bool) (int, []string) {
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
	refs := make([]transcriptRef, len(todo))
	errs := make([]error, len(todo))
	_ = each(ctx, len(todo), func(ctx context.Context, i int) error {
		refs[i], errs[i] = e.uploadTranscript(ctx, todo[i])
		return nil
	})
	n := 0
	var warnings []string
	for i, s := range todo {
		if errs[i] != nil {
			warnings = append(warnings, fmt.Sprintf("%s: transcript not stored (%v); retried next sync", s.Key(), errs[i]))
			continue
		}
		if refs[i].Name == "" {
			continue
		}
		rem := m.Sessions[s.Key()]
		refs[i].apply(&rem)
		m.Sessions[s.Key()] = rem
		n++
	}
	return n, warnings
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
	var packed []byte
	if rem.TranscriptParts <= 1 {
		_, packed, err = e.getOpen(ctx, e.transcriptKey(agent, id, 0))
	} else {
		for i := 1; i <= rem.TranscriptParts && err == nil; i++ {
			var part []byte
			_, part, err = e.getOpen(ctx, e.transcriptKey(agent, id, i))
			packed = append(packed, part...)
		}
	}
	if errors.Is(err, ErrObjectNotFound) {
		return "", nil, ErrNoTranscript
	}
	if err != nil {
		return "", nil, err
	}
	if rem.TranscriptSHA != "" && digestOf(packed) != rem.TranscriptSHA {
		return "", nil, errors.New("the vault's copy of this transcript is incomplete: sync again from the machine that has the session")
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
