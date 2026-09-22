package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
)

const providerDefsSchema = `
CREATE TABLE IF NOT EXISTS provider_defs (
	id         TEXT PRIMARY KEY,
	def        TEXT NOT NULL,
	updated_at TEXT NOT NULL
);`

// ProviderDefs returns every declared provider.
func (s *Store) ProviderDefs() ([]provider.Def, error) {
	rows, err := s.DB.Query(`SELECT def FROM provider_defs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query provider defs: %w", err)
	}
	defer rows.Close()
	out := []provider.Def{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d provider.Def
		if json.Unmarshal([]byte(raw), &d) == nil && d.ID != "" {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// PutProviderDef stores a def, replacing one with the same id.
func (s *Store) PutProviderDef(d provider.Def) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`INSERT INTO provider_defs (id, def, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET def = excluded.def, updated_at = excluded.updated_at`,
		d.ID, string(raw), time.Now().UTC().Format(time.RFC3339))
	return err
}

// DeleteProviderDef removes a def.
func (s *Store) DeleteProviderDef(id string) error {
	res, err := s.DB.Exec(`DELETE FROM provider_defs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
