package store

import (
	"fmt"
	"strings"
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
	// TestConn is the account that answered the last test.
	TestConn string `json:"testConn,omitempty"`
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

// migrateModels adds the columns newer than the table.
func (s *Store) migrateModels() error {
	if _, err := s.DB.Exec(`ALTER TABLE provider_models ADD COLUMN test_conn TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("add column test_conn: %w", err)
	}
	return nil
}

// SyncModels records the models a provider listed and returns the ones seen
// for the first time. A new model starts on when startOn says so. With live
// (the provider answered with its list), a stored model missing from the list
// is marked stale, not deleted, so the operator's on/off choice and the model's
// test history survive a provider that drops it for a while; a fallback list
// changes nothing. A model that reappears is un-marked on the next sync.
func (s *Store) SyncModels(provider string, ids []string, live bool, startOn func(model string) bool) ([]string, error) {
	// Read before the transaction: a read that turns into a write inside one
	// fails with SQLITE_BUSY_SNAPSHOT when another connection wrote meanwhile.
	known := map[string]bool{}
	rows, err := s.DB.Query(`SELECT model FROM provider_models WHERE provider = ?`, provider)
	if err != nil {
		return nil, fmt.Errorf("query models: %w", err)
	}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return nil, err
		}
		known[m] = true
	}
	rows.Close()
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	listed := map[string]bool{}
	var added []string
	for _, id := range ids {
		listed[id] = true
		if known[id] {
			if _, err := tx.Exec(`UPDATE provider_models SET stale = 0, last_seen = ? WHERE provider = ? AND model = ?`, now, provider, id); err != nil {
				return nil, fmt.Errorf("sync model: %w", err)
			}
			continue
		}
		on := 1
		if startOn != nil && !startOn(id) {
			on = 0
		}
		if _, err := tx.Exec(`INSERT INTO provider_models (provider, model, active, stale, first_seen, last_seen)
			VALUES (?, ?, ?, 0, ?, ?) ON CONFLICT(provider, model) DO NOTHING`, provider, id, on, now, now); err != nil {
			return nil, fmt.Errorf("sync model: %w", err)
		}
		known[id] = true
		added = append(added, id)
	}
	if live {
		for m := range known {
			if !listed[m] {
				if _, err := tx.Exec(`UPDATE provider_models SET stale = 1 WHERE provider = ? AND model = ?`, provider, m); err != nil {
					return nil, fmt.Errorf("mark model stale: %w", err)
				}
			}
		}
	}
	return added, tx.Commit()
}

// ListModels returns a provider's stored models by name.
func (s *Store) ListModels(provider string) ([]ProviderModel, error) {
	rows, err := s.DB.Query(`SELECT provider, model, active, stale, first_seen, last_seen, test_at, test_ok, test_ms, test_msg, test_conn
		FROM provider_models WHERE provider = ? ORDER BY model`, provider)
	if err != nil {
		return nil, fmt.Errorf("query models: %w", err)
	}
	defer rows.Close()
	out := []ProviderModel{}
	for rows.Next() {
		var m ProviderModel
		var active, stale, ok int
		if err := rows.Scan(&m.Provider, &m.Model, &active, &stale, &m.FirstSeen, &m.LastSeen, &m.TestAt, &ok, &m.TestMs, &m.TestMsg, &m.TestConn); err != nil {
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
func (s *Store) RecordModelTest(provider, model string, ok bool, ms int64, msg, conn string) error {
	v := 0
	if ok {
		v = 1
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	_, err := s.DB.Exec(`UPDATE provider_models SET test_at = ?, test_ok = ?, test_ms = ?, test_msg = ?, test_conn = ? WHERE provider = ? AND model = ?`,
		time.Now().UTC().Format(time.RFC3339), v, ms, msg, conn, provider, model)
	return err
}
