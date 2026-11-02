package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

// ErrConflict means another writer already used that (game, version).
var ErrConflict = errors.New("version conflict")

const sqliteConstraintUnique = 2067 // SQLITE_CONSTRAINT_UNIQUE

const createEvents = `
CREATE TABLE IF NOT EXISTS events (
  sequence   INTEGER PRIMARY KEY AUTOINCREMENT,
  game_id    TEXT NOT NULL,
  version    INTEGER NOT NULL,
  type       TEXT NOT NULL,
  data       TEXT NOT NULL,   -- JSON
  created_at TEXT NOT NULL,
  UNIQUE (game_id, version)
)`

// OpenDB opens the SQLite file, creating it and its directory if needed.
func OpenDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	// _txlock=immediate: a transaction takes the write lock when it begins, so an
	// outside writer cannot land between our read and our write.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(createEvents); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// EventStore is the append-only events table. Rows are never updated or deleted.
type EventStore struct{ db *sql.DB }

func NewEventStore(db *sql.DB) *EventStore { return &EventStore{db: db} }

// Append writes all events or none. A duplicate (game_id, version) returns ErrConflict.
func (s *EventStore) Append(ctx context.Context, events []Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, e := range events {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO events (game_id, version, type, data, created_at) VALUES (?, ?, ?, ?, ?)`,
			e.GameID, e.Version, e.Type, string(e.Data), time.Now().UTC().Format(time.RFC3339Nano))
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique {
			return ErrConflict
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Load returns one game's events in version order.
func (s *EventStore) Load(ctx context.Context, gameID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, eventColumns+` WHERE game_id = ? ORDER BY version`, gameID)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// LoadInTx is Load inside the caller's transaction, for projections.
func LoadInTx(ctx context.Context, tx *sql.Tx, gameID string) ([]Event, error) {
	rows, err := tx.QueryContext(ctx, eventColumns+` WHERE game_id = ? ORDER BY version`, gameID)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// After returns every game's events with a sequence greater than the given one,
// in sequence order, reading inside the caller's transaction.
func (s *EventStore) After(ctx context.Context, tx *sql.Tx, sequence int64) ([]Event, error) {
	rows, err := tx.QueryContext(ctx, eventColumns+` WHERE sequence > ? ORDER BY sequence`, sequence)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

const eventColumns = `SELECT sequence, game_id, version, type, data, created_at FROM events`

// scanEvents reads and closes rows, so the single connection is free afterwards.
func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var data, createdAt string
		if err := rows.Scan(&e.Sequence, &e.GameID, &e.Version, &e.Type, &data, &createdAt); err != nil {
			return nil, err
		}
		e.Data = []byte(data)
		t, err := parseTimestamp(createdAt)
		if err != nil {
			return nil, err
		}
		e.CreatedAt = t
		events = append(events, e)
	}
	return events, rows.Err()
}

// parseTimestamp reads our own RFC 3339 timestamps, and also the
// "2006-01-02 15:04:05" (UTC) that SQLite's datetime('now') writes when
// someone inserts a row from the sqlite3 shell.
func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02 15:04:05", s)
}
