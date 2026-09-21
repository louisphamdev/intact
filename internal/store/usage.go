package store

import "fmt"

// UsageRow is one day's token total for an account and model. It is the only
// usage record intact keeps: the daily row answers every question the dashboard
// asks, so the per-request rows that 9router stored are never written.
type UsageRow struct {
	Day          string `json:"day"`
	ConnectionID string `json:"connectionId"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	Requests     int64  `json:"requests"`
}

// AddUsage folds one request's token counts into the daily counter keyed by
// day, account and model. A second call on the same key adds to the row and
// counts another request.
func (s *Store) AddUsage(day, connID, model string, inputTokens, outputTokens int64) error {
	_, err := s.DB.Exec(
		`INSERT INTO usage_daily (day, connection_id, model, input_tokens, output_tokens, requests)
		 VALUES (?, ?, ?, ?, ?, 1)
		 ON CONFLICT(day, connection_id, model) DO UPDATE SET
		   input_tokens  = input_tokens  + excluded.input_tokens,
		   output_tokens = output_tokens + excluded.output_tokens,
		   requests      = requests      + 1`,
		day, connID, model, inputTokens, outputTokens)
	if err != nil {
		return fmt.Errorf("add usage: %w", err)
	}
	return nil
}

// Usage returns the daily counters, newest day first.
func (s *Store) Usage() ([]UsageRow, error) {
	rows, err := s.DB.Query(
		`SELECT day, connection_id, model, input_tokens, output_tokens, requests
		 FROM usage_daily ORDER BY day DESC, connection_id, model`)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer rows.Close()

	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Day, &r.ConnectionID, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.Requests); err != nil {
			return nil, fmt.Errorf("scan usage: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
