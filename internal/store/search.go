package store

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/EliasOlie/agyh/internal/model"
)

// Filter narrows List and Search.
type Filter struct {
	Agent       string
	Project     string
	Workspace   string // substring match
	IncludeSubs bool   // include sub-agent conversations
	Limit       int
}

// Hit is a session plus, for searches, a highlighted snippet.
type Hit struct {
	model.Session
	Snippet  string   `json:"snippet,omitempty"`
	Projects []string `json:"projects,omitempty"`
}

const sessionCols = `s.agent, s.id, s.title, s.preview, s.workspace, s.started_at, s.updated_at, s.messages, s.status, s.parent, s.path`

// projectSep joins project names inside one column (ASCII unit separator:
// project names are validated and can never contain it).
const projectSep = "\x1f"

// Every query selects the session columns, its projects (or NULL when the
// caller doesn't need them) and a snippet (or ”), in that order; see query.
// The projects subquery uses the project_sessions(agent, id) index, so only
// the returned rows pay for it.
const (
	colsWithProjects = sessionCols + `, (SELECT group_concat(p.project, char(31)) FROM project_sessions p WHERE p.agent = s.agent AND p.id = s.id)`
	colsNoProjects   = sessionCols + `, NULL`
)

func (f Filter) where(args *[]any) string {
	var conds []string
	if f.Agent != "" {
		conds = append(conds, "s.agent = ?")
		*args = append(*args, f.Agent)
	}
	if f.Project != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM project_sessions p WHERE p.project = ? AND p.agent = s.agent AND p.id = s.id)")
		*args = append(*args, f.Project)
	}
	if f.Workspace != "" {
		conds = append(conds, "s.workspace LIKE ?")
		*args = append(*args, "%"+f.Workspace+"%")
	}
	if !f.IncludeSubs {
		conds = append(conds, "s.parent = ''")
	}
	if len(conds) == 0 {
		return "1=1"
	}
	return strings.Join(conds, " AND ")
}

func (f Filter) limit() int {
	if f.Limit <= 0 {
		return 50
	}
	return f.Limit
}

// List returns sessions, most recently active first.
func (s *Store) List(ctx context.Context, f Filter) ([]Hit, error) {
	var args []any
	q := `SELECT ` + colsWithProjects + `, '' FROM sessions s WHERE ` + f.where(&args) + ` ORDER BY s.updated_at DESC LIMIT ?`
	args = append(args, f.limit())
	return s.query(ctx, q, args...)
}

// Search runs a full-text query. Every word must match (prefix match, accents ignored).
func (s *Store) Search(ctx context.Context, text string, f Filter) ([]Hit, error) {
	match := FTSQuery(text)
	if match == "" {
		return s.List(ctx, f)
	}
	args := []any{match}
	// FTS columns: 0 title, 1 workspace, 2 body.
	q := `SELECT ` + colsWithProjects + `, snippet(sessions_fts, 2, '[', ']', '…', 14)
		FROM sessions_fts f JOIN sessions s ON s.rid = f.rowid
		WHERE sessions_fts MATCH ? AND ` + f.where(&args) + `
		ORDER BY bm25(sessions_fts, 8.0, 2.0, 1.0) * ` + recencyBoost + `, s.updated_at DESC LIMIT ?`
	args = append(args, recencyArgs(time.Now())...)
	args = append(args, f.limit())
	return s.query(ctx, q, args...)
}

// Recency tiers: a match from this week outranks an equally good match from
// last quarter. bm25 is negative (lower is better), so a bigger multiplier
// pushes a session up. Tiers: 7d ×4, 15d ×3, 30d ×2, 90d ×1.5, older ×1.
const recencyBoost = `(CASE
	WHEN s.updated_at >= ? THEN 4.0
	WHEN s.updated_at >= ? THEN 3.0
	WHEN s.updated_at >= ? THEN 2.0
	WHEN s.updated_at >= ? THEN 1.5
	ELSE 1.0 END)`

func recencyArgs(now time.Time) []any {
	day := 24 * time.Hour
	return []any{now.Add(-7 * day).Unix(), now.Add(-15 * day).Unix(), now.Add(-30 * day).Unix(), now.Add(-90 * day).Unix()}
}

// FTSQuery turns free text into a safe FTS5 expression: each word becomes a
// quoted prefix term, so user input can never inject FTS operators.
// One-character words are dropped: as prefixes they match almost every
// document (and in Portuguese "a"/"e"/"o" are words), which made a single
// keystroke cost seconds.
func FTSQuery(text string) string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.'
	})
	var terms []string
	for _, w := range words {
		w = strings.Trim(w, "-.")
		if utf8.RuneCountInString(w) < 2 {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " ")
}

// query scans rows shaped as: session columns, projects (NULL or joined by
// projectSep), snippet.
func (s *Store) query(ctx context.Context, q string, args ...any) ([]Hit, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		var started, updated int64
		var projects sql.NullString
		if err := rows.Scan(&h.Agent, &h.ID, &h.Title, &h.Preview, &h.Workspace, &started, &updated,
			&h.Messages, &h.Status, &h.Parent, &h.Path, &projects, &h.Snippet); err != nil {
			return nil, err
		}
		h.StartedAt, h.UpdatedAt = fromUnix(started), fromUnix(updated)
		if projects.Valid && projects.String != "" {
			h.Projects = strings.Split(projects.String, projectSep)
			slices.Sort(h.Projects)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
