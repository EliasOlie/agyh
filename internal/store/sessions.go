package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/EliasOlie/agyh/internal/model"
)

// maxBodyBytes caps how much text of one session goes into the full-text index.
const maxBodyBytes = 256 << 10

// Item is a parsed session ready to be indexed.
type Item struct {
	Session  model.Session
	Messages []model.Message
}

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
	return s.UpsertMany(ctx, []Item{{Session: sess, Messages: msgs}})
}

// UpsertMany stores a batch in one transaction (much faster on first index).
func (s *Store) UpsertMany(ctx context.Context, items []Item) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	w, err := newSessionWriter(ctx, tx)
	if err != nil {
		return err
	}
	defer w.close()
	for _, it := range items {
		if err := w.upsert(ctx, it.Session, it.Messages); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// sessionWriter holds the prepared statements of one write transaction, so a
// batch compiles each statement once instead of once per session.
type sessionWriter struct {
	upsertMeta, deleteText, insertText *sql.Stmt
}

func newSessionWriter(ctx context.Context, tx *sql.Tx) (*sessionWriter, error) {
	w := &sessionWriter{}
	var err error
	prep := func(q string) *sql.Stmt {
		if err != nil {
			return nil
		}
		var st *sql.Stmt
		st, err = tx.PrepareContext(ctx, q)
		return st
	}
	// RETURNING yields the rowid for both the insert and the update path.
	w.upsertMeta = prep(`
		INSERT INTO sessions (agent, id, title, preview, workspace, started_at, updated_at, messages, status, parent, path, mtime)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent, id) DO UPDATE SET
			title=excluded.title, preview=excluded.preview, workspace=excluded.workspace,
			started_at=excluded.started_at, updated_at=excluded.updated_at, messages=excluded.messages,
			status=excluded.status, parent=excluded.parent, path=excluded.path, mtime=excluded.mtime
		RETURNING rid`)
	w.deleteText = prep(`DELETE FROM sessions_fts WHERE rowid = ?`)
	w.insertText = prep(`INSERT INTO sessions_fts (rowid, title, workspace, body) VALUES (?, ?, ?, ?)`)
	if err != nil {
		w.close()
		return nil, err
	}
	return w, nil
}

func (w *sessionWriter) close() {
	for _, st := range []*sql.Stmt{w.upsertMeta, w.deleteText, w.insertText} {
		if st != nil {
			st.Close()
		}
	}
}

func (w *sessionWriter) upsert(ctx context.Context, sess model.Session, msgs []model.Message) error {
	var rid int64
	if err := w.upsertMeta.QueryRowContext(ctx,
		sess.Agent, sess.ID, sess.Title, sess.Preview, sess.Workspace,
		unix(sess.StartedAt), unix(sess.UpdatedAt), sess.Messages, sess.Status, sess.Parent,
		sess.Path, sess.ModTime.UnixNano()).Scan(&rid); err != nil {
		return err
	}
	if _, err := w.deleteText.ExecContext(ctx, rid); err != nil {
		return err
	}
	_, err := w.insertText.ExecContext(ctx, rid, sess.Title, sess.Workspace, body(msgs, sess.Preview))
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

// DeleteMissing removes an agent's sessions whose source file no longer exists
// in keep. Sessions restored from the vault ("archive:", "synced:") or without
// a path are never removed: no agent file backs them.
func (s *Store) DeleteMissing(ctx context.Context, agent string, keep map[string]bool) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rid, path FROM sessions WHERE agent = ?`, agent)
	if err != nil {
		return 0, err
	}
	var gone []int64
	for rows.Next() {
		var rid int64
		var p string
		if err := rows.Scan(&rid, &p); err != nil {
			rows.Close()
			return 0, err
		}
		if p == "" || keep[p] || strings.HasPrefix(p, "archive:") || strings.HasPrefix(p, "synced:") {
			continue
		}
		gone = append(gone, rid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(gone) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	delText, err := tx.PrepareContext(ctx, `DELETE FROM sessions_fts WHERE rowid = ?`)
	if err != nil {
		return 0, err
	}
	defer delText.Close()
	delMeta, err := tx.PrepareContext(ctx, `DELETE FROM sessions WHERE rid = ?`)
	if err != nil {
		return 0, err
	}
	defer delMeta.Close()
	for _, rid := range gone {
		if _, err := delText.ExecContext(ctx, rid); err != nil {
			return 0, err
		}
		if _, err := delMeta.ExecContext(ctx, rid); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(gone), nil
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
	q := `SELECT ` + colsNoProjects + `, '' FROM sessions s WHERE s.id LIKE ?`
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

// AllSessions returns every session in the store without a limit, ordered by updated_at DESC.
func (s *Store) AllSessions(ctx context.Context) ([]model.Session, error) {
	hits, err := s.query(ctx, `SELECT `+colsNoProjects+`, '' FROM sessions s ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	out := make([]model.Session, len(hits))
	for i, h := range hits {
		out[i] = h.Session
	}
	return out, nil
}

// Children are the sessions an agent started from this one (subagents).
func (s *Store) Children(ctx context.Context, agent, id string) ([]model.Session, error) {
	hits, err := s.query(ctx, `SELECT `+colsNoProjects+`, '' FROM sessions s WHERE s.agent = ? AND s.parent = ?`, agent, id)
	if err != nil {
		return nil, err
	}
	out := make([]model.Session, len(hits))
	for i, h := range hits {
		out[i] = h.Session
	}
	return out, nil
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
