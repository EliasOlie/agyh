// Package store is agyh's local index: one SQLite file with session metadata,
// a full-text index over every message (FTS5, accent-insensitive) and the
// user's projects. Agent data is only ever read; this is the only file agyh writes.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/EliasOlie/agyh/internal/model"

	_ "modernc.org/sqlite"
)

// maxBodyBytes caps how much text of one session goes into the full-text index.
const maxBodyBytes = 256 << 10

var ErrNotFound = errors.New("not found")

// ErrAmbiguous is returned when an ID prefix matches more than one session.
type ErrAmbiguous struct{ Matches []model.Session }

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("ambiguous id: %d sessions match, use more characters", len(e.Matches))
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	agent      TEXT NOT NULL,
	id         TEXT NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	preview    TEXT NOT NULL DEFAULT '',
	workspace  TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0,
	messages   INTEGER NOT NULL DEFAULT 0,
	status     TEXT NOT NULL DEFAULT '',
	parent     TEXT NOT NULL DEFAULT '',
	path       TEXT NOT NULL DEFAULT '',
	mtime      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (agent, id)
);
CREATE INDEX IF NOT EXISTS sessions_updated ON sessions(updated_at DESC);
CREATE INDEX IF NOT EXISTS sessions_path ON sessions(path);
CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
	agent UNINDEXED, id UNINDEXED, title, workspace, body,
	tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TABLE IF NOT EXISTS projects (
	name        TEXT PRIMARY KEY,
	description TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS project_sessions (
	project  TEXT NOT NULL REFERENCES projects(name) ON DELETE CASCADE ON UPDATE CASCADE,
	agent    TEXT NOT NULL,
	id       TEXT NOT NULL,
	note     TEXT NOT NULL DEFAULT '',
	added_at INTEGER NOT NULL,
	PRIMARY KEY (project, agent, id)
);
`

// DefaultPath is $AGYH_DB, else $XDG_DATA_HOME/agyh/index.db, else ~/.local/share/agyh/index.db.
func DefaultPath() string {
	if v := os.Getenv("AGYH_DB"); v != "" {
		return v
	}
	return filepath.Join(DataDir(), "index.db")
}

// DataDir is where agyh keeps its own state.
func DataDir() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "agyh")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "agyh")
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer; also keeps pragmas on a single connection
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Mtimes returns path → indexed modification time for one agent.
func (s *Store) Mtimes(ctx context.Context, agent string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, mtime FROM sessions WHERE agent = ?`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var m int64
		if err := rows.Scan(&p, &m); err != nil {
			return nil, err
		}
		out[p] = m
	}
	return out, rows.Err()
}

// Upsert stores one session and replaces its full-text entry.
func (s *Store) Upsert(ctx context.Context, sess model.Session, msgs []model.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertTx(ctx, tx, sess, msgs); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertMany stores a batch in one transaction (much faster on first index).
func (s *Store) UpsertMany(ctx context.Context, items []Item) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, it := range items {
		if err := upsertTx(ctx, tx, it.Session, it.Messages); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Item is a parsed session ready to be indexed.
type Item struct {
	Session  model.Session
	Messages []model.Message
}

func upsertTx(ctx context.Context, tx *sql.Tx, sess model.Session, msgs []model.Message) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (agent, id, title, preview, workspace, started_at, updated_at, messages, status, parent, path, mtime)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent, id) DO UPDATE SET
			title=excluded.title, preview=excluded.preview, workspace=excluded.workspace,
			started_at=excluded.started_at, updated_at=excluded.updated_at, messages=excluded.messages,
			status=excluded.status, parent=excluded.parent, path=excluded.path, mtime=excluded.mtime`,
		sess.Agent, sess.ID, sess.Title, sess.Preview, sess.Workspace,
		unix(sess.StartedAt), unix(sess.UpdatedAt), sess.Messages, sess.Status, sess.Parent,
		sess.Path, sess.ModTime.UnixNano())
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions_fts WHERE agent = ? AND id = ?`, sess.Agent, sess.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions_fts (agent, id, title, workspace, body) VALUES (?, ?, ?, ?, ?)`,
		sess.Agent, sess.ID, sess.Title, sess.Workspace, body(msgs, sess.Preview))
	return err
}

func body(msgs []model.Message, preview string) string {
	var b strings.Builder
	b.WriteString(preview)
	for _, m := range msgs {
		if b.Len() >= maxBodyBytes {
			break
		}
		b.WriteByte('\n')
		b.WriteString(m.Text)
	}
	out := b.String()
	if len(out) > maxBodyBytes {
		out = out[:maxBodyBytes]
	}
	return out
}

// DeleteMissing removes an agent's sessions whose source file no longer exists in keep.
func (s *Store) DeleteMissing(ctx context.Context, agent string, keep map[string]bool) (int, error) {
	idx, err := s.Mtimes(ctx, agent)
	if err != nil {
		return 0, err
	}
	n := 0
	for p := range idx {
		if keep[p] {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE agent = ? AND path = ?`, agent, p)
		if err != nil {
			return n, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE agent = ? AND id = ?`, agent, id); err != nil {
				return n, err
			}
			if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions_fts WHERE agent = ? AND id = ?`, agent, id); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

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
	q := `SELECT ` + sessionCols + `, '' FROM sessions s WHERE ` + f.where(&args) + ` ORDER BY s.updated_at DESC LIMIT ?`
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
	q := `SELECT ` + sessionCols + `, snippet(sessions_fts, 4, '[', ']', '…', 14)
		FROM sessions_fts f JOIN sessions s ON s.agent = f.agent AND s.id = f.id
		WHERE sessions_fts MATCH ? AND ` + f.where(&args) + `
		ORDER BY bm25(sessions_fts, 0, 0, 8.0, 2.0, 1.0) * ` + recencyBoost + `, s.updated_at DESC LIMIT ?`
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
func FTSQuery(text string) string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.'
	})
	var terms []string
	for _, w := range words {
		w = strings.Trim(w, "-.")
		if w == "" {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " ")
}

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
		if err := rows.Scan(&h.Agent, &h.ID, &h.Title, &h.Preview, &h.Workspace, &started, &updated,
			&h.Messages, &h.Status, &h.Parent, &h.Path, &h.Snippet); err != nil {
			return nil, err
		}
		h.StartedAt, h.UpdatedAt = fromUnix(started), fromUnix(updated)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, s.attachProjects(ctx, hits)
}

func (s *Store) attachProjects(ctx context.Context, hits []Hit) error {
	if len(hits) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT project, agent, id FROM project_sessions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	byKey := map[string][]string{}
	for rows.Next() {
		var p, a, id string
		if err := rows.Scan(&p, &a, &id); err != nil {
			return err
		}
		byKey[a+":"+id] = append(byKey[a+":"+id], p)
	}
	for i := range hits {
		hits[i].Projects = byKey[hits[i].Key()]
	}
	return rows.Err()
}

// Resolve finds a session by "agent:id", full id, or an id prefix (min 4 chars).
func (s *Store) Resolve(ctx context.Context, ref string) (model.Session, error) {
	agent, id := "", strings.TrimSpace(ref)
	if a, rest, ok := strings.Cut(id, ":"); ok {
		agent, id = a, rest
	}
	if len(id) < 4 {
		return model.Session{}, fmt.Errorf("id %q too short: use at least 4 characters", id)
	}
	args := []any{strings.ReplaceAll(id, "%", "") + "%"}
	q := `SELECT ` + sessionCols + `, '' FROM sessions s WHERE s.id LIKE ?`
	if agent != "" {
		q += ` AND s.agent = ?`
		args = append(args, agent)
	}
	hits, err := s.query(ctx, q+` ORDER BY s.updated_at DESC LIMIT 10`, args...)
	if err != nil {
		return model.Session{}, err
	}
	for _, h := range hits { // exact match wins over prefix matches
		if h.ID == id {
			return h.Session, nil
		}
	}
	switch len(hits) {
	case 0:
		return model.Session{}, ErrNotFound
	case 1:
		return hits[0].Session, nil
	}
	ms := make([]model.Session, len(hits))
	for i, h := range hits {
		ms[i] = h.Session
	}
	return model.Session{}, &ErrAmbiguous{Matches: ms}
}

// Latest returns the most recently active top-level session (optionally for one agent).
func (s *Store) Latest(ctx context.Context, agent string) (model.Session, error) {
	hits, err := s.List(ctx, Filter{Agent: agent, Limit: 1})
	if err != nil {
		return model.Session{}, err
	}
	if len(hits) == 0 {
		return model.Session{}, ErrNotFound
	}
	return hits[0].Session, nil
}

// Count returns the number of indexed sessions per agent.
func (s *Store) Count(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent, count(*) FROM sessions GROUP BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			return nil, err
		}
		out[a] = n
	}
	return out, rows.Err()
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}
