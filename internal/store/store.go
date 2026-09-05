// Package store persists bot state in SQLite so it survives restarts.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	// Pure-Go SQLite driver, so the bot builds without cgo.
	_ "modernc.org/sqlite"
)

// Store holds authorized user and chat IDs.
type Store struct {
	db *sql.DB
}

// Entry is one authorized ID.
type Entry struct {
	ID      int64
	AddedBy int64
	AddedAt time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS authorized_ids (
	id       INTEGER PRIMARY KEY,
	added_by INTEGER NOT NULL,
	added_at INTEGER NOT NULL
);`

// Open opens (and if needed creates) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// One connection: SQLite serialises writers anyway, and a second pooled
	// connection only turns a concurrent read into "database is locked".
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema in %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// IsAuthorized reports whether any of the given IDs is authorized. Passing both
// a user and a chat ID authorizes the command if either was granted access.
func (s *Store) IsAuthorized(ids ...int64) (bool, error) {
	for _, id := range ids {
		if id == 0 {
			continue
		}
		var exists int
		err := s.db.QueryRow("SELECT 1 FROM authorized_ids WHERE id = ?", id).Scan(&exists)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("look up authorization: %w", err)
		}
	}
	return false, nil
}

// Authorize grants access to an ID. It reports false when the ID was already
// authorized, so callers can tell the user nothing changed.
func (s *Store) Authorize(id, addedBy int64) (bool, error) {
	result, err := s.db.Exec(
		"INSERT OR IGNORE INTO authorized_ids (id, added_by, added_at) VALUES (?, ?, ?)",
		id, addedBy, time.Now().Unix(),
	)
	if err != nil {
		return false, fmt.Errorf("authorize %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("authorize %d: %w", id, err)
	}
	return affected > 0, nil
}

// Revoke removes access. It reports false when the ID was not authorized.
func (s *Store) Revoke(id int64) (bool, error) {
	result, err := s.db.Exec("DELETE FROM authorized_ids WHERE id = ?", id)
	if err != nil {
		return false, fmt.Errorf("revoke %d: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke %d: %w", id, err)
	}
	return affected > 0, nil
}

// List returns every authorized ID, most recently added first.
func (s *Store) List() ([]Entry, error) {
	rows, err := s.db.Query("SELECT id, added_by, added_at FROM authorized_ids ORDER BY added_at DESC")
	if err != nil {
		return nil, fmt.Errorf("list authorizations: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var entry Entry
		var addedAt int64
		if err := rows.Scan(&entry.ID, &entry.AddedBy, &addedAt); err != nil {
			return nil, fmt.Errorf("list authorizations: %w", err)
		}
		entry.AddedAt = time.Unix(addedAt, 0)
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
