package source

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/EliasOlie/agyh/internal/model"
)

// Codex reads OpenAI Codex CLI history: rollout-*.jsonl files under
// ~/.codex/sessions/YYYY/MM/DD/.
type Codex struct{ root string }

func NewCodex(root string) *Codex { return &Codex{root: root} }

func (c *Codex) Name() string    { return "codex" }
func (c *Codex) Available() bool { return dirExists(c.root) }

func (c *Codex) Refs(ctx context.Context) ([]Ref, error) {
	var refs []Ref
	err := filepath.WalkDir(c.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, don't fail the whole scan
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		refs = append(refs, Ref{Agent: "codex", ID: codexIDFromName(d.Name()), Path: p, ModTime: info.ModTime()})
		return ctx.Err()
	})
	return refs, err
}

// rollout-2026-01-02T10-11-12-<uuid>.jsonl → <uuid>
func codexIDFromName(name string) string {
	stem := strings.TrimSuffix(name, ".jsonl")
	if len(stem) >= 36 {
		return stem[len(stem)-36:]
	}
	return stem
}

type codexRecord struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		ID      string          `json:"id"`
		Cwd     string          `json:"cwd"`
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"payload"`
}

var codexNoisePrefixes = []string{"<environment_context>", "<user_instructions>", "# AGENTS.md instructions"}

func (c *Codex) Load(ctx context.Context, ref Ref) (model.Session, []model.Message, error) {
	s := model.Session{Agent: "codex", ID: ref.ID, Path: ref.Path, ModTime: ref.ModTime}
	var msgs []model.Message
	err := eachJSONLine(ctx, ref.Path, func(raw []byte) error {
		var r codexRecord
		if json.Unmarshal(raw, &r) != nil {
			return nil
		}
		at := parseTime(r.Timestamp)
		if !at.IsZero() {
			if s.StartedAt.IsZero() || at.Before(s.StartedAt) {
				s.StartedAt = at
			}
			if at.After(s.UpdatedAt) {
				s.UpdatedAt = at
			}
		}
		switch {
		case r.Type == "session_meta":
			if r.Payload.ID != "" {
				s.ID = r.Payload.ID
			}
			s.Workspace = r.Payload.Cwd
		case r.Type == "response_item" && r.Payload.Type == "message":
			role := r.Payload.Role
			if role != "user" && role != "assistant" {
				return nil
			}
			txt := textFromContent(r.Payload.Content)
			if txt == "" || (role == "user" && hasAnyPrefix(txt, codexNoisePrefixes)) {
				return nil
			}
			msgs = append(msgs, model.Message{Role: role, Text: txt, At: at})
		}
		return nil
	})
	if err != nil {
		return s, nil, err
	}
	s.Title = firstUserText(msgs)
	s.Preview = lastAssistantText(msgs, 160)
	s.Messages = len(msgs)
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = ref.ModTime
	}
	return s, msgs, nil
}

func (c *Codex) Resume(s model.Session) (string, []string) {
	return s.Workspace, []string{"codex", "resume", s.ID}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
