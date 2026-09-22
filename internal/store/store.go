// Package store keeps provider credentials in a local SQLite database.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store owns the database handle.
type Store struct {
	DB *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS connections (
	id          TEXT PRIMARY KEY,
	provider    TEXT NOT NULL,
	label       TEXT NOT NULL,
	secret      TEXT NOT NULL,
	is_active   INTEGER NOT NULL DEFAULT 1,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS usage_daily (
	day           TEXT NOT NULL,
	connection_id TEXT NOT NULL,
	model         TEXT NOT NULL,
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	requests      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, connection_id, model)
);`

// Open opens the database at path and applies the schema.
// WAL keeps a reader from blocking the writer, which matters because the
// dashboard reads while requests are being served.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal=WAL&_timeout=5000&_sync=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if _, err := db.Exec(modelsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply models schema: %w", err)
	}
	if _, err := db.Exec(pendingSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply pending schema: %w", err)
	}
	if _, err := db.Exec(driftSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply drift schema: %w", err)
	}
	if _, err := db.Exec(apiKeySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply api key schema: %w", err)
	}
	if _, err := db.Exec(filterSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply filter schema: %w", err)
	}
	st := &Store{DB: db}
	if err := st.migrateOAuth(); err != nil {
		db.Close()
		return nil, err
	}
	if err := st.seedFilters(); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed filters: %w", err)
	}
	return st, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.DB.Close() }
