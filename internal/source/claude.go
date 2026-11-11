package source

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/EliasOlie/agyh/internal/model"
)

// Claude reads Claude Code history: one JSONL file per session under
// ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl.
type Claude struct{ root string }

func NewClaude(root string) *Claude { return &Claude{root: root} }

func (c *Claude) Name() string    { return "claude" }
func (c *Claude) Available() bool { return dirExists(c.root) }

func (c *Claude) Refs(ctx context.Context) ([]Ref, error) {
	// Only top-level files: subagent transcripts live in nested folders.
	files, err := filepath.Glob(filepath.Join(c.root, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, len(files))
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		refs = append(refs, Ref{Agent: "claude", ID: id, Path: f, ModTime: st.ModTime()})
	}
	return refs, nil
}

type claudeRecord struct {
	Type        string `json:"type"`
	Cwd         string `json:"cwd"`
	Timestamp   string `json:"timestamp"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	CustomTitle string `json:"customTitle"`
	Summary     string `json:"summary"`
	Message     *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (c *Claude) Load(ctx context.Context, ref Ref) (model.Session, []model.Message, error) {
	s := model.Session{Agent: "claude", ID: ref.ID, Path: ref.Path, ModTime: ref.ModTime}
	var (
		msgs                 []model.Message
		customTitle, summary string
	)
	err := eachJSONLine(ctx, ref.Path, func(raw []byte) error {
		var r claudeRecord
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
		if s.Workspace == "" && r.Cwd != "" {
			s.Workspace = r.Cwd
		}
		switch r.Type {
		case "custom-title":
			customTitle = r.CustomTitle
		case "summary":
			if summary == "" {
				summary = r.Summary
			}
		case "user", "assistant":
			if r.IsMeta || r.IsSidechain || r.Message == nil {
				return nil
			}
			txt := textFromContent(r.Message.Content)
			if r.Type == "user" && isClaudeNoise(txt) {
				return nil
			}
			if txt != "" {
				msgs = append(msgs, model.Message{Role: r.Type, Text: txt, At: at})
			}
		}
		return nil
	})
	if err != nil {
		return s, nil, err
	}
	s.Title = firstNonEmpty(customTitle, summary, firstUserText(msgs))
	s.Preview = lastAssistantText(msgs, 160)
	s.Messages = len(msgs)
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = ref.ModTime
	}
	return s, msgs, nil
}

func (c *Claude) Resume(s model.Session) (string, []string) {
	return s.Workspace, []string{"claude", "--resume", s.ID}
}

// textFromContent accepts either a plain string or a list of content blocks
// and keeps only the text blocks (tool calls and results are dropped).
func textFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text", "input_text", "output_text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

var claudeNoisePrefixes = []string{
	"<command-name>", "<command-message>", "<command-args>", "<local-command-stdout>",
	"<local-command-stderr>", "<local-command-caveat>", "<bash-input>", "<bash-stdout>",
	"<bash-stderr>", "<task-notification>", "<system-reminder>", "Caveat:", "[Request interrupted",
}

func isClaudeNoise(txt string) bool {
	for _, p := range claudeNoisePrefixes {
		if strings.HasPrefix(txt, p) {
			return true
		}
	}
	return false
}

func lastAssistantText(msgs []model.Message, n int) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return model.Truncate(msgs[i].Text, n)
		}
	}
	return ""
}
