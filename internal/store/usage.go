package store

import (
	"fmt"
	"strings"
)

// UsageRow is one day's token total for an account and model. It is the only
// usage record intact keeps: the daily row answers every question the dashboard
// asks, so the per-request rows that 9router stored are never written.
type UsageRow struct {
	Day          string `json:"day"`
	ConnectionID string `json:"connectionId"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	CachedTokens int64  `json:"cachedTokens"`
	Requests     int64  `json:"requests"`
}

// AddUsage folds one request's token counts into the daily counter keyed by
// day, account and model. A second call on the same key adds to the row and
// counts another request. cachedTokens are the part of inputTokens read from
// the provider's cache.
func (s *Store) AddUsage(day, connID, model string, inputTokens, outputTokens, cachedTokens int64) error {
	_, err := s.DB.Exec(
		`INSERT INTO usage_daily (day, connection_id, model, input_tokens, output_tokens, cached_tokens, requests)
		 VALUES (?, ?, ?, ?, ?, ?, 1)
		 ON CONFLICT(day, connection_id, model) DO UPDATE SET
		   input_tokens  = input_tokens  + excluded.input_tokens,
		   output_tokens = output_tokens + excluded.output_tokens,
		   cached_tokens = cached_tokens + excluded.cached_tokens,
		   requests      = requests      + 1`,
		day, connID, model, inputTokens, outputTokens, cachedTokens)
	if err != nil {
		return fmt.Errorf("add usage: %w", err)
	}
	return nil
}

// Usage returns the daily counters, newest day first.
func (s *Store) Usage() ([]UsageRow, error) {
	rows, err := s.DB.Query(
		`SELECT day, connection_id, model, input_tokens, output_tokens, cached_tokens, requests
		 FROM usage_daily ORDER BY day DESC, connection_id, model`)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer rows.Close()

	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Day, &r.ConnectionID, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.Requests); err != nil {
			return nil, fmt.Errorf("scan usage: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// migrateUsageCached adds the cached_tokens column to a database made before it.
func (s *Store) migrateUsageCached() error {
	for _, table := range []string{"usage_daily", "usage_key_daily"} {
		if _, err := s.DB.Exec("ALTER TABLE " + table + " ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0"); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("add column cached_tokens to %s: %w", table, err)
		}
	}
	return nil
}
