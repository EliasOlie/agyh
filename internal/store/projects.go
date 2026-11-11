package store

import (
	"context"
	"fmt"
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
	return err
}

// EnsureProject creates the project if it does not exist yet.
func (s *Store) EnsureProject(ctx context.Context, name string) error {
	if err := ValidProjectName(name); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO projects (name, description, created_at) VALUES (?, '', ?)`,
		name, time.Now().Unix())
	return err
}

func (s *Store) DeleteProject(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RenameProject(ctx context.Context, from, to string) error {
	if err := ValidProjectName(to); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET name = ? WHERE name = ?`, to, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AddToProject(ctx context.Context, project string, sess model.Session, note string) error {
	if err := s.EnsureProject(ctx, project); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_sessions (project, agent, id, note, added_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project, agent, id) DO UPDATE SET note = CASE WHEN excluded.note <> '' THEN excluded.note ELSE note END`,
		project, sess.Agent, sess.ID, note, time.Now().Unix())
	return err
}

func (s *Store) RemoveFromProject(ctx context.Context, project string, sess model.Session) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM project_sessions WHERE project = ? AND agent = ? AND id = ?`,
		project, sess.Agent, sess.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.name, p.description, p.created_at, count(ps.id), coalesce(max(s.updated_at), 0)
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
		if err := rows.Scan(&p.Name, &p.Description, &created, &p.Sessions, &last); err != nil {
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
