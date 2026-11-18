package store

import (
	"context"
	"database/sql"
	"fmt"
)

// The full-text table is keyed by the session's rowid (sessions.rid), so
// replacing or deleting a session's text is a rowid lookup. Filtering an FTS5
// table by an UNINDEXED column is a full scan, which made indexing quadratic.
//
// rid is an INTEGER PRIMARY KEY on purpose: an implicit rowid may be
// renumbered by VACUUM, which would silently detach the full-text rows.
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	rid        INTEGER PRIMARY KEY,
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
	UNIQUE (agent, id)
);
CREATE INDEX IF NOT EXISTS sessions_updated ON sessions(updated_at DESC);
CREATE INDEX IF NOT EXISTS sessions_path ON sessions(path);
CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
	title, workspace, body,
	tokenize = 'unicode61 remove_diacritics 2',
	prefix = '2 3 4'
);
CREATE TABLE IF NOT EXISTS projects (
	name        TEXT PRIMARY KEY,
	description TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	color       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS project_sessions (
	project  TEXT NOT NULL REFERENCES projects(name) ON DELETE CASCADE ON UPDATE CASCADE,
	agent    TEXT NOT NULL,
	id       TEXT NOT NULL,
	note     TEXT NOT NULL DEFAULT '',
	added_at INTEGER NOT NULL,
	PRIMARY KEY (project, agent, id)
);
CREATE INDEX IF NOT EXISTS project_sessions_session ON project_sessions(agent, id);
CREATE TABLE IF NOT EXISTS pending_projects (
	agent     TEXT NOT NULL,
	workspace TEXT NOT NULL,
	project   TEXT NOT NULL REFERENCES projects(name) ON DELETE CASCADE ON UPDATE CASCADE,
	since     INTEGER NOT NULL
);
`

// schemaVersion bumps when the index layout changes. Projects are user data
// and always survive.
//
//   - v1 → v2: FTS gained a prefix index. Derived tables are dropped and the
//     next sync refills them from the agents.
//   - v2 → v3: FTS rows are keyed by sessions.rid. Migrated in place, text
//     included, because sessions restored from the vault ("archive:" paths)
//     are not re-read by a sync and would otherwise vanish from search.
const schemaVersion = 3

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return nil
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var stmts []string
	if v == 2 && tableExists(ctx, tx, "sessions") {
		stmts = []string{
			// Free the index names (they follow a renamed table), then move the old tables aside.
			`DROP INDEX IF EXISTS sessions_updated`,
			`DROP INDEX IF EXISTS sessions_path`,
			`ALTER TABLE sessions RENAME TO sessions_v2`,
			`ALTER TABLE sessions_fts RENAME TO sessions_fts_v2`,
			schema,
			`INSERT INTO sessions (agent, id, title, preview, workspace, started_at, updated_at, messages, status, parent, path, mtime)
				SELECT agent, id, title, preview, workspace, started_at, updated_at, messages, status, parent, path, mtime
				FROM sessions_v2`,
			`INSERT INTO sessions_fts (rowid, title, workspace, body)
				SELECT s.rid, f.title, f.workspace, f.body
				FROM sessions_fts_v2 f JOIN sessions s ON s.agent = f.agent AND s.id = f.id`,
			`DROP TABLE sessions_fts_v2`,
			`DROP TABLE sessions_v2`,
		}
	} else {
		stmts = []string{`DROP TABLE IF EXISTS sessions_fts`, `DROP TABLE IF EXISTS sessions`}
	}
	stmts = append(stmts, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func tableExists(ctx context.Context, tx *sql.Tx, name string) bool {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return err == nil && n > 0
}

// addColumns brings tables created by older versions up to date with
// additive changes (no rebuild, user data untouched).
func addColumns(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('projects')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	if !have["color"] {
		if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN color TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}
