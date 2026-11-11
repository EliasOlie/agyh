package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/EliasOlie/agyh/internal/model"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// Agy reads Google Antigravity CLI history: metadata from
// conversation_summaries.db and messages from brain/<id>/.system_generated/logs/transcript.jsonl.
type Agy struct{ root string }

func NewAgy(root string) *Agy { return &Agy{root: root} }

func (a *Agy) Name() string { return "agy" }

func (a *Agy) dbPath() string { return filepath.Join(a.root, "conversation_summaries.db") }

func (a *Agy) transcript(id string) string {
	return filepath.Join(a.root, "brain", id, ".system_generated", "logs", "transcript.jsonl")
}

func (a *Agy) Available() bool { return fileExists(a.dbPath()) }

func (a *Agy) Refs(ctx context.Context) ([]Ref, error) {
	db, err := sql.Open("sqlite", "file:"+a.dbPath()+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
		SELECT conversation_id, title, preview, step_count, last_modified_time,
		       last_user_input_time, workspace_uris, status, parent_conversation_id
		FROM conversation_summaries`)
	if err != nil {
		return nil, fmt.Errorf("agy: read summaries: %w", err)
	}
	defer rows.Close()

	var refs []Ref
	for rows.Next() {
		var (
			id, title, preview, ws, status, parent string
			steps                                  int
			modified, lastInput                    any
		)
		if err := rows.Scan(&id, &title, &preview, &steps, &modified, &lastInput, &ws, &status, &parent); err != nil {
			return nil, err
		}
		updated := sqlTime(lastInput)
		if mt := sqlTime(modified); mt.After(updated) {
			updated = mt
		}
		meta := &model.Session{
			Agent:     "agy",
			ID:        id,
			Title:     title,
			Preview:   preview,
			Workspace: workspaceFromURIs(ws),
			UpdatedAt: updated,
			Messages:  steps,
			Status:    strings.TrimPrefix(status, "CASCADE_RUN_STATUS_"),
			Parent:    parent,
		}
		ref := Ref{Agent: "agy", ID: id, Path: a.transcript(id), ModTime: updated, Meta: meta}
		if st, err := os.Stat(ref.Path); err == nil && st.ModTime().After(ref.ModTime) {
			ref.ModTime = st.ModTime()
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

type agyStep struct {
	Type      string `json:"type"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

func (a *Agy) Load(ctx context.Context, ref Ref) (model.Session, []model.Message, error) {
	s := model.Session{Agent: "agy", ID: ref.ID, Path: ref.Path, ModTime: ref.ModTime}
	if ref.Meta != nil {
		s = *ref.Meta
		s.Path, s.ModTime = ref.Path, ref.ModTime
	}
	var msgs []model.Message
	if fileExists(ref.Path) {
		err := eachJSONLine(ctx, ref.Path, func(raw []byte) error {
			var st agyStep
			if json.Unmarshal(raw, &st) != nil {
				return nil
			}
			at := parseTime(st.CreatedAt)
			switch st.Type {
			case "USER_INPUT":
				if txt := agyUserText(st.Content); txt != "" {
					msgs = append(msgs, model.Message{Role: "user", Text: txt, At: at})
				}
			case "PLANNER_RESPONSE":
				if txt := strings.TrimSpace(st.Content); txt != "" {
					msgs = append(msgs, model.Message{Role: "assistant", Text: txt, At: at})
				}
			}
			if !at.IsZero() && (s.StartedAt.IsZero() || at.Before(s.StartedAt)) {
				s.StartedAt = at
			}
			return nil
		})
		if err != nil {
			return s, nil, err
		}
	}
	if s.Title == "" {
		s.Title = firstUserText(msgs)
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = s.UpdatedAt
	}
	return s, msgs, nil
}

func (a *Agy) Resume(s model.Session) (string, []string) {
	return s.Workspace, []string{"agy", "--conversation", s.ID}
}

// agyUserText extracts what the user typed from the <USER_REQUEST> envelope.
func agyUserText(content string) string {
	const open, closeTag = "<USER_REQUEST>", "</USER_REQUEST>"
	if i := strings.Index(content, open); i >= 0 {
		rest := content[i+len(open):]
		if j := strings.Index(rest, closeTag); j >= 0 {
			rest = rest[:j]
		}
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(content)
}

// workspaceFromURIs turns "file:///home/me/proj" (possibly a JSON list or
// several URIs) into the first local path.
func workspaceFromURIs(v string) string {
	v = strings.TrimSpace(v)
	var list []string
	if strings.HasPrefix(v, "[") && json.Unmarshal([]byte(v), &list) == nil && len(list) > 0 {
		v = list[0]
	} else if f := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }); len(f) > 0 {
		v = f[0]
	}
	if u, err := url.Parse(v); err == nil && u.Scheme == "file" {
		return u.Path
	}
	return v
}

func sqlTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		return parseTime(t)
	case []byte:
		return parseTime(string(t))
	}
	return time.Time{}
}

func firstUserText(msgs []model.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			return model.Truncate(m.Text, 80)
		}
	}
	return ""
}
