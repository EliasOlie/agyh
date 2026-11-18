package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/EliasOlie/agyh/internal/model"
)

// ProjectKey normalizes a name to spot near-duplicates: "Conecta-Me",
// "conectame" and "conecta_me" share a key.
func ProjectKey(name string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	s, _, _ := transform.String(t, strings.ToLower(name))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SimilarProject returns an existing project whose name differs from name
// but normalizes to the same key ("" if none).
func (s *Store) SimilarProject(ctx context.Context, name string) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM projects`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	key := ProjectKey(name)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return "", err
		}
		if n != name && ProjectKey(n) == key {
			return n, nil
		}
	}
	return "", rows.Err()
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func tombstoneProject(ctx context.Context, tx *sql.Tx, name string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO project_tombstones (name, deleted_at, synced) VALUES (?, ?, 0)
		ON CONFLICT(name) DO UPDATE SET deleted_at = max(deleted_at, excluded.deleted_at), synced = 0`, name, at.Unix())
	return err
}

func clearProjectTombstone(ctx context.Context, tx *sql.Tx, name string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM project_tombstones WHERE name = ?`, name)
	return err
}

// MergeProjects moves every session of from into into (created if needed),
// keeps notes (into's note wins when both have one) and pending sessions,
// then deletes from. Sessions are never deleted. It returns how many
// sessions were moved.
func (s *Store) MergeProjects(ctx context.Context, from, into string) (int, error) {
	if from == into {
		return 0, errors.New("cannot merge a project into itself")
	}
	if err := ValidProjectName(into); err != nil {
		return 0, err
	}
	moved := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE name = ?`, from).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		now := time.Now()
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO projects (name, description, created_at)
			SELECT ?, description, ? FROM projects WHERE name = ?`, into, now.Unix(), from); err != nil {
			return err
		}
		if err := clearProjectTombstone(ctx, tx, into); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO project_sessions (project, agent, id, note, added_at)
			SELECT ?, agent, id, note, ? FROM project_sessions WHERE project = ?
			ON CONFLICT(project, agent, id) DO UPDATE SET note = CASE WHEN note = '' THEN excluded.note ELSE note END`,
			into, now.Unix(), from)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		moved = int(n)
		if _, err := tx.ExecContext(ctx, `UPDATE pending_projects SET project = ? WHERE project = ?`, into, from); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, from); err != nil {
			return err
		}
		return tombstoneProject(ctx, tx, from, now)
	})
	return moved, err
}

// Tombstones is what deletions the vault must carry, keyed by project name
// and by MembershipKey.
type Tombstones struct {
	Projects    map[string]time.Time `json:"projects,omitempty"`
	Memberships map[string]time.Time `json:"memberships,omitempty"`
}

// MembershipKey identifies one session in one project.
func MembershipKey(project, agent, id string) string { return project + "\x1f" + agent + ":" + id }

func splitMembership(k string) (project, agent, id string, ok bool) {
	project, rest, ok := strings.Cut(k, "\x1f")
	if !ok {
		return "", "", "", false
	}
	agent, id, ok = strings.Cut(rest, ":")
	return project, agent, id, ok
}

// LocalTombstones returns this machine's tombstones.
func (s *Store) LocalTombstones(ctx context.Context) (Tombstones, error) {
	t := Tombstones{Projects: map[string]time.Time{}, Memberships: map[string]time.Time{}}
	rows, err := s.db.QueryContext(ctx, `SELECT name, deleted_at FROM project_tombstones`)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var n string
		var at int64
		if err := rows.Scan(&n, &at); err != nil {
			rows.Close()
			return t, err
		}
		t.Projects[n] = time.Unix(at, 0)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT project, agent, id, removed_at FROM membership_tombstones`)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var p, a, id string
		var at int64
		if err := rows.Scan(&p, &a, &id, &at); err != nil {
			return t, err
		}
		t.Memberships[MembershipKey(p, a, id)] = time.Unix(at, 0)
	}
	return t, rows.Err()
}

// ApplyTombstones reconciles this machine with the vault's tombstones.
// Newest change wins: a tombstone deletes a project or membership created
// before it (sessions are never touched) and is recorded locally; a local
// project or membership created after it is kept and reported in revived,
// so the vault can drop that tombstone. Local tombstones already shared
// with the vault that it no longer carries were revived elsewhere: they are
// cleared. All local tombstones are then marked shared.
func (s *Store) ApplyTombstones(ctx context.Context, remote Tombstones) (revived Tombstones, err error) {
	revived = Tombstones{Projects: map[string]time.Time{}, Memberships: map[string]time.Time{}}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		for name, at := range remote.Projects {
			var created int64
			err := tx.QueryRowContext(ctx, `SELECT created_at FROM projects WHERE name = ?`, name).Scan(&created)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return err
			case created > at.Unix():
				revived.Projects[name] = at
				continue
			default:
				if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, name); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_tombstones (name, deleted_at, synced) VALUES (?, ?, 1)
				ON CONFLICT(name) DO UPDATE SET deleted_at = max(deleted_at, excluded.deleted_at), synced = 1`, name, at.Unix()); err != nil {
				return err
			}
		}
		for key, at := range remote.Memberships {
			p, a, id, ok := splitMembership(key)
			if !ok {
				continue
			}
			var added int64
			err := tx.QueryRowContext(ctx, `SELECT added_at FROM project_sessions WHERE project = ? AND agent = ? AND id = ?`, p, a, id).Scan(&added)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return err
			case added > at.Unix():
				revived.Memberships[key] = at
				continue
			default:
				if _, err := tx.ExecContext(ctx, `DELETE FROM project_sessions WHERE project = ? AND agent = ? AND id = ?`, p, a, id); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO membership_tombstones (project, agent, id, removed_at, synced) VALUES (?, ?, ?, ?, 1)
				ON CONFLICT(project, agent, id) DO UPDATE SET removed_at = max(removed_at, excluded.removed_at), synced = 1`, p, a, id, at.Unix()); err != nil {
				return err
			}
		}
		// Shared before, gone from the vault now: revived on another machine.
		rows, err := tx.QueryContext(ctx, `SELECT name FROM project_tombstones WHERE synced = 1`)
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			if _, ok := remote.Projects[n]; !ok {
				stale = append(stale, n)
			}
		}
		rows.Close()
		for _, n := range stale {
			if _, err := tx.ExecContext(ctx, `DELETE FROM project_tombstones WHERE name = ?`, n); err != nil {
				return err
			}
		}
		rows, err = tx.QueryContext(ctx, `SELECT project, agent, id FROM membership_tombstones WHERE synced = 1`)
		if err != nil {
			return err
		}
		var staleM [][3]string
		for rows.Next() {
			var p, a, id string
			rows.Scan(&p, &a, &id)
			if _, ok := remote.Memberships[MembershipKey(p, a, id)]; !ok {
				staleM = append(staleM, [3]string{p, a, id})
			}
		}
		rows.Close()
		for _, m := range staleM {
			if _, err := tx.ExecContext(ctx, `DELETE FROM membership_tombstones WHERE project = ? AND agent = ? AND id = ?`, m[0], m[1], m[2]); err != nil {
				return err
			}
		}
		return nil
	})
	return revived, err
}

// MarkTombstonesShared records that the vault now carries every local tombstone.
func (s *Store) MarkTombstonesShared(ctx context.Context) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE project_tombstones SET synced = 1`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE membership_tombstones SET synced = 1`)
		return err
	})
}

// RestoreMembership adds a membership that came from the vault, unless this
// machine has a tombstone for the project or the membership. It reports
// whether it was added.
func (s *Store) RestoreMembership(ctx context.Context, project string, sess model.Session, note string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM project_tombstones WHERE name = ?) +
		(SELECT count(*) FROM membership_tombstones WHERE project = ? AND agent = ? AND id = ?)`,
		project, project, sess.Agent, sess.ID).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if err := ValidProjectName(project); err != nil {
		return false, fmt.Errorf("vault project %q: %w", project, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (name, description, created_at) VALUES (?, '', ?)`,
		project, time.Now().Unix()); err != nil {
		return false, err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_sessions (project, agent, id, note, added_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project, agent, id) DO UPDATE SET note = CASE WHEN excluded.note <> '' THEN excluded.note ELSE note END`,
		project, sess.Agent, sess.ID, note, time.Now().Unix())
	return err == nil, err
}

// ErrSimilarProject: a new project name looks like an existing one
// ("conectame" vs "conecta-me"), which is how duplicates are born.
type ErrSimilarProject struct{ Name, Existing string }

func (e *ErrSimilarProject) Error() string {
	return fmt.Sprintf("project %q looks like the existing %q", e.Name, e.Existing)
}

// CheckNewProject returns *ErrSimilarProject when name doesn't exist yet but
// a project with a near-identical name does. Existing names pass.
func (s *Store) CheckNewProject(ctx context.Context, name string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE name = ?`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	similar, err := s.SimilarProject(ctx, name)
	if err != nil {
		return err
	}
	if similar != "" {
		return &ErrSimilarProject{Name: name, Existing: similar}
	}
	return nil
}
