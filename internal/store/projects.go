package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/EliasOlie/agyh/internal/model"
)

// Project groups sessions from any agent under a name.
type Project struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Sessions    int       `json:"sessions"`
	LastActive  time.Time `json:"last_active"`
	// Color is the user's choice ("" = derived from the name, see internal/colors).
	Color string `json:"color,omitempty"`
}

var projectName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._-]{0,63}$`)

// ValidProjectName keeps names easy to type in a terminal and in chat commands.
func ValidProjectName(name string) error {
	if !projectName.MatchString(name) {
		return fmt.Errorf("invalid project name %q: use letters, digits, '.', '_' or '-' (max 64)", name)
	}
	return nil
}

func (s *Store) CreateProject(ctx context.Context, name, desc string) error {
	if err := ValidProjectName(name); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO projects (name, description, created_at) VALUES (?, ?, ?)`,
		name, desc, time.Now().Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("project %q already exists", name)
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM project_tombstones WHERE name = ?`, name)
	return err
}

// EnsureProject creates the project if it does not exist yet.
func (s *Store) EnsureProject(ctx context.Context, name string) error {
	if err := ValidProjectName(name); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (name, description, created_at) VALUES (?, '', ?)`,
		name, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 { // (re)created now: newer than any deletion
		_, err = s.db.ExecContext(ctx, `DELETE FROM project_tombstones WHERE name = ?`, name)
	}
	return err
}

// DeleteProject removes a project and its memberships; sessions are kept.
// A tombstone keeps the vault from bringing it back.
func (s *Store) DeleteProject(ctx context.Context, name string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return tombstoneProject(ctx, tx, name, time.Now())
	})
}

func (s *Store) RenameProject(ctx context.Context, from, to string) error {
	if err := ValidProjectName(to); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE projects SET name = ?, created_at = ? WHERE name = ?`, to, time.Now().Unix(), from)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		// Other machines know the old name: it is gone, the new one is new.
		if err := clearProjectTombstone(ctx, tx, to); err != nil {
			return err
		}
		return tombstoneProject(ctx, tx, from, time.Now())
	})
}

func (s *Store) AddToProject(ctx context.Context, project string, sess model.Session, note string) error {
	if err := s.EnsureProject(ctx, project); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_sessions (project, agent, id, note, added_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project, agent, id) DO UPDATE SET note = CASE WHEN excluded.note <> '' THEN excluded.note ELSE note END`,
		project, sess.Agent, sess.ID, note, time.Now().Unix())
	if err != nil {
		return err
	}
	// Added again by the user: newer than any earlier removal.
	_, err = s.db.ExecContext(ctx, `DELETE FROM membership_tombstones WHERE project = ? AND agent = ? AND id = ?`, project, sess.Agent, sess.ID)
	return err
}

func (s *Store) RemoveFromProject(ctx context.Context, project string, sess model.Session) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM project_sessions WHERE project = ? AND agent = ? AND id = ?`,
			project, sess.Agent, sess.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO membership_tombstones (project, agent, id, removed_at, synced) VALUES (?, ?, ?, ?, 0)
			ON CONFLICT(project, agent, id) DO UPDATE SET removed_at = excluded.removed_at, synced = 0`,
			project, sess.Agent, sess.ID, time.Now().Unix())
		return err
	})
}

func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.name, p.description, p.created_at, count(ps.id), coalesce(max(s.updated_at), 0), p.color
		FROM projects p
		LEFT JOIN project_sessions ps ON ps.project = p.name
		LEFT JOIN sessions s ON s.agent = ps.agent AND s.id = ps.id
		GROUP BY p.name
		ORDER BY coalesce(max(s.updated_at), p.created_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var created, last int64
		if err := rows.Scan(&p.Name, &p.Description, &created, &p.Sessions, &last, &p.Color); err != nil {
			return nil, err
		}
		p.CreatedAt, p.LastActive = fromUnix(created), fromUnix(last)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Notes returns the note attached to each session of a project, keyed by "agent:id".
func (s *Store) Notes(ctx context.Context, project string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent, id, note FROM project_sessions WHERE project = ?`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, id, note string
		if err := rows.Scan(&a, &id, &note); err != nil {
			return nil, err
		}
		out[a+":"+id] = note
	}
	return out, rows.Err()
}

// ProjectMap returns all project names and notes for each session, keyed by "agent:id".
func (s *Store) ProjectMap(ctx context.Context) (map[string][]string, map[string]map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project, agent, id, note FROM project_sessions`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	projects := map[string][]string{}
	notes := map[string]map[string]string{}
	for rows.Next() {
		var p, a, id, note string
		if err := rows.Scan(&p, &a, &id, &note); err != nil {
			return nil, nil, err
		}
		key := a + ":" + id
		projects[key] = append(projects[key], p)
		if note != "" {
			if notes[key] == nil {
				notes[key] = map[string]string{}
			}
			notes[key][p] = note
		}
	}
	return projects, notes, rows.Err()
}

// SetProjectColor stores a project's color ("" goes back to the derived one).
func (s *Store) SetProjectColor(ctx context.Context, name, color string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET color = ? WHERE name = ?`, color, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ProjectColors maps every project to its stored color ("" when derived).
func (s *Store) ProjectColors(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, color FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, c string
		if err := rows.Scan(&n, &c); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, rows.Err()
}

// pendingTTL bounds how long a new session may take to show up in the index.
const pendingTTL = 24 * time.Hour

// AddPendingProject remembers that a session about to be started by agent in
// workspace belongs to project. Its id is not known until the agent writes
// it; ResolvePending assigns it once the index has it.
func (s *Store) AddPendingProject(ctx context.Context, agent, workspace, project string, since time.Time) error {
	if err := s.EnsureProject(ctx, project); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pending_projects (agent, workspace, project, since) VALUES (?, ?, ?, ?)`,
		agent, filepath.Clean(workspace), project, since.Unix())
	return err
}

// ResolvePending adds each pending session to its project: the first
// top-level session of that agent in that workspace that started at or after
// the request (and isn't in the project yet). Requests older than a day are
// dropped. It returns how many sessions were assigned.
func (s *Store) ResolvePending(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pending_projects WHERE since < ?`, time.Now().Add(-pendingTTL).Unix()); err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, agent, workspace, project, since FROM pending_projects ORDER BY since`)
	if err != nil {
		return 0, err
	}
	type pending struct {
		rid                       int64
		agent, workspace, project string
		since                     int64
	}
	var all []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.rid, &p.agent, &p.workspace, &p.project, &p.since); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, p)
	}
	rows.Close()
	n := 0
	for _, p := range all {
		var id string
		err := s.db.QueryRowContext(ctx, `
			SELECT s.id FROM sessions s
			WHERE s.agent = ? AND s.workspace = ? AND s.parent = '' AND (s.started_at >= ? OR s.started_at = 0 AND s.updated_at >= ?)
			  AND NOT EXISTS (SELECT 1 FROM project_sessions ps WHERE ps.agent = s.agent AND ps.id = s.id AND ps.project = ?)
			ORDER BY s.started_at, s.updated_at LIMIT 1`,
			p.agent, p.workspace, p.since-60, p.since-60, p.project).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return n, err
		}
		if err := s.AddToProject(ctx, p.project, model.Session{Agent: p.agent, ID: id}, ""); err != nil {
			return n, err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM pending_projects WHERE rowid = ?`, p.rid); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
