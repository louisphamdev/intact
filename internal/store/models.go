package store

import (
	"fmt"
	"time"
)

// ProviderModel is one model a provider lists, with the operator's switch and
// the last test result.
type ProviderModel struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Active    bool   `json:"active"`
	Stale     bool   `json:"stale"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
	TestAt    string `json:"testAt,omitempty"`
	TestOK    bool   `json:"testOk"`
	TestMs    int64  `json:"testMs,omitempty"`
	TestMsg   string `json:"testMsg,omitempty"`
}

const modelsSchema = `
CREATE TABLE IF NOT EXISTS provider_models (
	provider   TEXT NOT NULL,
	model      TEXT NOT NULL,
	active     INTEGER NOT NULL DEFAULT 1,
	stale      INTEGER NOT NULL DEFAULT 0,
	first_seen TEXT NOT NULL,
	last_seen  TEXT NOT NULL,
	test_at    TEXT NOT NULL DEFAULT '',
	test_ok    INTEGER NOT NULL DEFAULT 0,
	test_ms    INTEGER NOT NULL DEFAULT 0,
	test_msg   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (provider, model)
);`

// SyncModels records the models a provider listed. A new model starts active.
// With markStale, a stored model missing from the list is marked stale (kept,
// so the operator decides whether to delete it); a listed one is fresh again.
func (s *Store) SyncModels(provider string, ids []string, markStale bool) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if markStale {
		if _, err := tx.Exec(`UPDATE provider_models SET stale = 1 WHERE provider = ?`, provider); err != nil {
			return fmt.Errorf("mark stale: %w", err)
		}
	}
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT INTO provider_models (provider, model, active, stale, first_seen, last_seen)
			VALUES (?, ?, 1, 0, ?, ?)
			ON CONFLICT(provider, model) DO UPDATE SET stale = 0, last_seen = excluded.last_seen`,
			provider, id, now, now); err != nil {
			return fmt.Errorf("sync model: %w", err)
		}
	}
	return tx.Commit()
}

// ListModels returns a provider's stored models by name.
func (s *Store) ListModels(provider string) ([]ProviderModel, error) {
	rows, err := s.DB.Query(`SELECT provider, model, active, stale, first_seen, last_seen, test_at, test_ok, test_ms, test_msg
		FROM provider_models WHERE provider = ? ORDER BY model`, provider)
	if err != nil {
		return nil, fmt.Errorf("query models: %w", err)
	}
	defer rows.Close()
	out := []ProviderModel{}
	for rows.Next() {
		var m ProviderModel
		var active, stale, ok int
		if err := rows.Scan(&m.Provider, &m.Model, &active, &stale, &m.FirstSeen, &m.LastSeen, &m.TestAt, &ok, &m.TestMs, &m.TestMsg); err != nil {
			return nil, err
		}
		m.Active, m.Stale, m.TestOK = active != 0, stale != 0, ok != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// InactiveModels returns the models of a provider the operator switched off.
func (s *Store) InactiveModels(provider string) map[string]bool {
	out := map[string]bool{}
	rows, err := s.DB.Query(`SELECT model FROM provider_models WHERE provider = ? AND active = 0`, provider)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		rows.Scan(&m)
		out[m] = true
	}
	return out
}

// SetModelsActive switches models on or off.
func (s *Store) SetModelsActive(provider string, models []string, active bool) (int64, error) {
	v := 0
	if active {
		v = 1
	}
	var n int64
	for _, m := range models {
		res, err := s.DB.Exec(`UPDATE provider_models SET active = ? WHERE provider = ? AND model = ?`, v, provider, m)
		if err != nil {
			return n, err
		}
		k, _ := res.RowsAffected()
		n += k
	}
	return n, nil
}

// DeleteModels forgets models; a model the provider still lists comes back
// on the next fetch.
func (s *Store) DeleteModels(provider string, models []string) (int64, error) {
	var n int64
	for _, m := range models {
		res, err := s.DB.Exec(`DELETE FROM provider_models WHERE provider = ? AND model = ?`, provider, m)
		if err != nil {
			return n, err
		}
		k, _ := res.RowsAffected()
		n += k
	}
	return n, nil
}

// RecordModelTest stores the result of a test call.
func (s *Store) RecordModelTest(provider, model string, ok bool, ms int64, msg string) error {
	v := 0
	if ok {
		v = 1
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	_, err := s.DB.Exec(`UPDATE provider_models SET test_at = ?, test_ok = ?, test_ms = ?, test_msg = ? WHERE provider = ? AND model = ?`,
		time.Now().UTC().Format(time.RFC3339), v, ms, msg, provider, model)
	return err
}
