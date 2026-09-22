package store

import (
	"errors"
	"fmt"
	"time"
)

// Filter is one rule that removes something from a request before it leaves
// for a provider. Provider "*" applies it to every provider. The kinds are
// described in package filter.
type Filter struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Pattern  string `json:"pattern"`
	Note     string `json:"note"`
	Enabled  bool   `json:"enabled"`
}

// ErrFilterNotFound reports that no filter has the requested id.
var ErrFilterNotFound = errors.New("filter not found")

const filterSchema = `
CREATE TABLE IF NOT EXISTS filters (
	id         TEXT PRIMARY KEY,
	provider   TEXT NOT NULL DEFAULT '*',
	kind       TEXT NOT NULL,
	pattern    TEXT NOT NULL,
	note       TEXT NOT NULL DEFAULT '',
	enabled    INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`

// defaultFilters are the fixes first found against real providers. They are
// written once, on the first open, and can then be edited or deleted.
var defaultFilters = []Filter{
	{Provider: "*", Kind: "schema", Pattern: "encrypted", Note: "Cursor/MCP tool schemas; strict providers reject it"},
	{Provider: "*", Kind: "schema", Pattern: "cache_control", Note: "not a JSON Schema keyword"},
	{Provider: "*", Kind: "schema", Pattern: "$id", Note: "JSON Schema meta keyword some providers reject"},
	{Provider: "*", Kind: "schema", Pattern: "example", Note: "non-standard annotation"},
	{Provider: "antigravity", Kind: "system", Pattern: `^x-anthropic-billing-header:.*$`,
		Note: "Claude Code fingerprint; Antigravity answers a false 429 when it sees it"},
}

// seedFilters writes the default filters the first time a database is opened.
func (s *Store) seedFilters() error {
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'filters_seeded'`).Scan(&n); err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	if n > 0 {
		return nil
	}
	for _, f := range defaultFilters {
		f.Enabled = true
		if _, err := s.SaveFilter(f); err != nil {
			return err
		}
	}
	_, err := s.DB.Exec(`INSERT INTO settings (key, value) VALUES ('filters_seeded', '1')`)
	return err
}

// ListFilters returns every filter, oldest first.
func (s *Store) ListFilters() ([]Filter, error) {
	rows, err := s.DB.Query(`SELECT id, provider, kind, pattern, note, enabled FROM filters ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("query filters: %w", err)
	}
	defer rows.Close()
	out := []Filter{}
	for rows.Next() {
		var f Filter
		var en int
		if err := rows.Scan(&f.ID, &f.Provider, &f.Kind, &f.Pattern, &f.Note, &en); err != nil {
			return nil, fmt.Errorf("scan filter: %w", err)
		}
		f.Enabled = en != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// SaveFilter creates a filter when f.ID is empty, or replaces the one with that
// id, and returns the stored filter.
func (s *Store) SaveFilter(f Filter) (Filter, error) {
	if f.Provider == "" {
		f.Provider = "*"
	}
	en := 0
	if f.Enabled {
		en = 1
	}
	if f.ID == "" {
		id, err := newID()
		if err != nil {
			return Filter{}, err
		}
		f.ID = id
		_, err = s.DB.Exec(`INSERT INTO filters (id, provider, kind, pattern, note, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			f.ID, f.Provider, f.Kind, f.Pattern, f.Note, en, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return Filter{}, fmt.Errorf("insert filter: %w", err)
		}
		return f, nil
	}
	res, err := s.DB.Exec(`UPDATE filters SET provider = ?, kind = ?, pattern = ?, note = ?, enabled = ? WHERE id = ?`,
		f.Provider, f.Kind, f.Pattern, f.Note, en, f.ID)
	if err != nil {
		return Filter{}, fmt.Errorf("update filter: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Filter{}, ErrFilterNotFound
	}
	return f, nil
}

// DeleteFilter removes one filter.
func (s *Store) DeleteFilter(id string) error {
	res, err := s.DB.Exec(`DELETE FROM filters WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete filter: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFilterNotFound
	}
	return nil
}
