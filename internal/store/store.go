// Package store is agyh's local index: one SQLite file with session metadata,
// a full-text index over every message (FTS5, accent-insensitive) and the
// user's projects. Agent data is only ever read; this is the only file agyh writes.
//
// The package is split by concern: schema.go (tables and migrations),
// sessions.go (writing and resolving sessions), search.go (List and Search)
// and projects.go (user-defined projects).
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/EliasOlie/agyh/internal/model"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

// ErrAmbiguous is returned when an ID prefix matches more than one session.
type ErrAmbiguous struct{ Matches []model.Session }

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("ambiguous id: %d sessions match, use more characters", len(e.Matches))
}

type Store struct{ db *sql.DB }

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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate index: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := addColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("update schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
