package source

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/EliasOlie/agyh/internal/model"
)

// ArchivePayload is the serialized representation of a session stored locally or in cloud sync.
type ArchivePayload struct {
	Session  model.Session   `json:"session"`
	Messages []model.Message `json:"messages"`
}

// ArchiveDir returns where synced session archives are kept locally.
func ArchiveDir() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "agyh", "archive")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "agyh", "archive")
}

// ArchivePath returns the file path for an archived session.
func ArchivePath(agent, id string) string {
	return filepath.Join(ArchiveDir(), agent, id+".json.gz")
}

// SaveArchive writes a session and its messages to a local gzip-compressed file.
func SaveArchive(sess model.Session, msgs []model.Message) (string, error) {
	target := ArchivePath(sess.Agent, sess.ID)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer func() {
		f.Close()
		_ = os.Remove(tmp)
	}()

	gw := gzip.NewWriter(f)
	if err := json.NewEncoder(gw).Encode(ArchivePayload{Session: sess, Messages: msgs}); err != nil {
		_ = gw.Close()
		return "", err
	}
	if err := gw.Close(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", err
	}
	return target, nil
}

// LoadArchive reads an archived session file.
func LoadArchive(path string) (model.Session, []model.Message, error) {
	cleanPath := strings.TrimPrefix(path, "archive:")
	f, err := os.Open(cleanPath)
	if err != nil {
		return model.Session{}, nil, err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return model.Session{}, nil, fmt.Errorf("open gzip archive: %w", err)
	}
	defer gr.Close()

	var payload ArchivePayload
	if err := json.NewDecoder(gr).Decode(&payload); err != nil {
		return model.Session{}, nil, fmt.Errorf("decode archive: %w", err)
	}
	return payload.Session, payload.Messages, nil
}

// LoadSessionOrArchive loads messages from the original agent source file, falling
// back to the local archive if the file does not exist (e.g. session synced from another machine).
func LoadSessionOrArchive(ctx context.Context, sess model.Session) (model.Session, []model.Message, error) {
	if strings.HasPrefix(sess.Path, "archive:") {
		return LoadArchive(sess.Path)
	}
	src := ByName(sess.Agent)
	if src != nil {
		meta := sess
		s, msgs, err := src.Load(ctx, Ref{Agent: sess.Agent, ID: sess.ID, Path: sess.Path, Meta: &meta})
		if err == nil && len(msgs) > 0 {
			return s, msgs, nil
		}
	}
	// Fallback to archive if available
	archPath := ArchivePath(sess.Agent, sess.ID)
	if _, err := os.Stat(archPath); err == nil {
		return LoadArchive(archPath)
	}
	if src == nil {
		return sess, nil, fmt.Errorf("unknown agent %q", sess.Agent)
	}
	return sess, nil, fmt.Errorf("session file not found and no local archive exists")
}
