package store

import (
	"fmt"
	"time"
)

// UpstreamError is one failed answer (or failed call) of a provider, kept
// for analysis: every attempt, including the ones intact failed over from.
type UpstreamError struct {
	ID         int64   `json:"id"`
	At         string  `json:"at"`
	Provider   string  `json:"provider"`
	Connection string  `json:"connectionId"`
	Model      string  `json:"model"`
	Client     string  `json:"client"`
	Endpoint   string  `json:"endpoint"`
	Status     int     `json:"status"` // 0: the call itself failed (network, timeout)
	LatencyMs  int64   `json:"latencyMs"`
	Class      string  `json:"class"`
	Signature  string  `json:"signature"`
	Message    string  `json:"message"`
	QuotaLeft  float64 `json:"quotaLeft"` // 0–1 of the model's quota when read; -1 unknown
	Headers    string  `json:"headers,omitempty"`
	RespBody   string  `json:"respBody,omitempty"`
	ReqBody    string  `json:"reqBody,omitempty"`
}

const errorsSchema = `
CREATE TABLE IF NOT EXISTS upstream_errors (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	at          TEXT NOT NULL,
	provider    TEXT NOT NULL,
	connection  TEXT NOT NULL DEFAULT '',
	model       TEXT NOT NULL DEFAULT '',
	client      TEXT NOT NULL DEFAULT '',
	endpoint    TEXT NOT NULL DEFAULT '',
	status      INTEGER NOT NULL,
	latency_ms  INTEGER NOT NULL DEFAULT 0,
	class       TEXT NOT NULL DEFAULT '',
	signature   TEXT NOT NULL DEFAULT '',
	message     TEXT NOT NULL DEFAULT '',
	quota_left  REAL NOT NULL DEFAULT -1,
	headers     TEXT NOT NULL DEFAULT '',
	resp_body   TEXT NOT NULL DEFAULT '',
	req_body    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS upstream_errors_at ON upstream_errors (at);
CREATE INDEX IF NOT EXISTS upstream_errors_prov ON upstream_errors (provider, at);`

// AddUpstreamError stores an error and returns its id.
func (s *Store) AddUpstreamError(e UpstreamError) (int64, error) {
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}
	res, err := s.DB.Exec(`INSERT INTO upstream_errors (at, provider, connection, model, client, endpoint, status, latency_ms,
		class, signature, message, quota_left, headers, resp_body, req_body) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.At, e.Provider, e.Connection, e.Model, e.Client, e.Endpoint, e.Status, e.LatencyMs, e.Class, e.Signature,
		e.Message, e.QuotaLeft, e.Headers, e.RespBody, e.ReqBody)
	if err != nil {
		return 0, fmt.Errorf("add upstream error: %w", err)
	}
	return res.LastInsertId()
}

// SetErrorQuota records the quota read after an error, and its class.
func (s *Store) SetErrorQuota(id int64, left float64, class string) error {
	_, err := s.DB.Exec(`UPDATE upstream_errors SET quota_left = ?, class = ? WHERE id = ?`, left, class, id)
	return err
}

// ErrorFilter narrows ListUpstreamErrors.
type ErrorFilter struct {
	Provider, Class, Signature, Since string
	Status                            int
	Limit                             int
	Full                              bool // include bodies
}

// ListUpstreamErrors returns errors, newest first.
func (s *Store) ListUpstreamErrors(f ErrorFilter) ([]UpstreamError, error) {
	cols := `id, at, provider, connection, model, client, endpoint, status, latency_ms, class, signature, message, quota_left, headers`
	if f.Full {
		cols += `, resp_body, req_body`
	}
	q := `SELECT ` + cols + ` FROM upstream_errors WHERE 1=1`
	var args []any
	for _, c := range []struct {
		col, v string
	}{{"provider", f.Provider}, {"class", f.Class}, {"signature", f.Signature}} {
		if c.v != "" {
			q += ` AND ` + c.col + ` = ?`
			args = append(args, c.v)
		}
	}
	if f.Status != 0 {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Since != "" {
		q += ` AND at >= ?`
		args = append(args, f.Since)
	}
	if f.Limit <= 0 || f.Limit > 5000 {
		f.Limit = 500
	}
	q += ` ORDER BY at DESC, id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query upstream errors: %w", err)
	}
	defer rows.Close()
	out := []UpstreamError{}
	for rows.Next() {
		var e UpstreamError
		dst := []any{&e.ID, &e.At, &e.Provider, &e.Connection, &e.Model, &e.Client, &e.Endpoint, &e.Status, &e.LatencyMs,
			&e.Class, &e.Signature, &e.Message, &e.QuotaLeft, &e.Headers}
		if f.Full {
			dst = append(dst, &e.RespBody, &e.ReqBody)
		}
		if err := rows.Scan(dst...); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetUpstreamError returns one error with its bodies.
func (s *Store) GetUpstreamError(id int64) (UpstreamError, error) {
	var e UpstreamError
	err := s.DB.QueryRow(`SELECT id, at, provider, connection, model, client, endpoint, status, latency_ms, class, signature,
		message, quota_left, headers, resp_body, req_body FROM upstream_errors WHERE id = ?`, id).Scan(&e.ID, &e.At, &e.Provider,
		&e.Connection, &e.Model, &e.Client, &e.Endpoint, &e.Status, &e.LatencyMs, &e.Class, &e.Signature, &e.Message,
		&e.QuotaLeft, &e.Headers, &e.RespBody, &e.ReqBody)
	return e, err
}

// PruneUpstreamErrors keeps the errors of the last days, at most max rows.
func (s *Store) PruneUpstreamErrors(days, max int) error {
	cut := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	if _, err := s.DB.Exec(`DELETE FROM upstream_errors WHERE at < ?`, cut); err != nil {
		return err
	}
	_, err := s.DB.Exec(`DELETE FROM upstream_errors WHERE id <= (SELECT id FROM upstream_errors ORDER BY id DESC LIMIT 1 OFFSET ?)`, max)
	return err
}

// CountUpstreamErrorsSince counts a provider's errors since a time.
func (s *Store) CountUpstreamErrorsSince(provider, since string) int {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM upstream_errors WHERE provider = ? AND at >= ? AND status != 0`, provider, since).Scan(&n)
	return n
}

// ErrorVerdict is what the error review decided for one group of errors (a
// provider and a signature). The newest verdict of a group is the one that
// counts; older ones are kept as its history.
type ErrorVerdict struct {
	ID        int64  `json:"id"`
	At        string `json:"at"`
	Provider  string `json:"provider"`
	Signature string `json:"signature"`
	Action    string `json:"action"`  // blacklist, disable_model, disable_account, ignore
	Applied   bool   `json:"applied"` // the action was carried out
	Verified  bool   `json:"verified"`
	Detail    string `json:"detail"` // the rule, model or account acted on
	Cause     string `json:"cause"`
	Reason    string `json:"reason"` // the model's reason
	Note      string `json:"note"`   // what intact checked and did
	By        string `json:"by"`
	Errors    int    `json:"errors"` // errors of the group when judged
	LastError string `json:"lastError"`
}

const errorVerdictsSchema = `
CREATE TABLE IF NOT EXISTS error_verdicts (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	at         TEXT NOT NULL,
	provider   TEXT NOT NULL,
	signature  TEXT NOT NULL,
	action     TEXT NOT NULL,
	applied    INTEGER NOT NULL DEFAULT 0,
	verified   INTEGER NOT NULL DEFAULT 0,
	detail     TEXT NOT NULL DEFAULT '',
	cause      TEXT NOT NULL DEFAULT '',
	reason     TEXT NOT NULL DEFAULT '',
	note       TEXT NOT NULL DEFAULT '',
	by_model   TEXT NOT NULL DEFAULT '',
	errors     INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS error_verdicts_group ON error_verdicts (provider, signature, id);`

// AddErrorVerdict records a verdict.
func (s *Store) AddErrorVerdict(v ErrorVerdict) (ErrorVerdict, error) {
	if v.At == "" {
		v.At = time.Now().UTC().Format(time.RFC3339)
	}
	res, err := s.DB.Exec(`INSERT INTO error_verdicts (at, provider, signature, action, applied, verified, detail, cause, reason,
		note, by_model, errors, last_error) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.At, v.Provider, v.Signature, v.Action, v.Applied,
		v.Verified, v.Detail, v.Cause, v.Reason, v.Note, v.By, v.Errors, v.LastError)
	if err != nil {
		return v, err
	}
	v.ID, _ = res.LastInsertId()
	return v, nil
}

// ErrorVerdicts returns the newest verdict of each group, keyed
// provider + "|" + signature.
func (s *Store) ErrorVerdicts() (map[string]ErrorVerdict, error) {
	rows, err := s.DB.Query(`SELECT id, at, provider, signature, action, applied, verified, detail, cause, reason, note, by_model,
		errors, last_error FROM error_verdicts WHERE id IN (SELECT MAX(id) FROM error_verdicts GROUP BY provider, signature)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ErrorVerdict{}
	for rows.Next() {
		var v ErrorVerdict
		if err := rows.Scan(&v.ID, &v.At, &v.Provider, &v.Signature, &v.Action, &v.Applied, &v.Verified, &v.Detail, &v.Cause,
			&v.Reason, &v.Note, &v.By, &v.Errors, &v.LastError); err != nil {
			return nil, err
		}
		out[v.Provider+"|"+v.Signature] = v
	}
	return out, rows.Err()
}

// ListErrorVerdicts returns the verdicts, newest first.
func (s *Store) ListErrorVerdicts(limit int) ([]ErrorVerdict, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.DB.Query(`SELECT id, at, provider, signature, action, applied, verified, detail, cause, reason, note, by_model,
		errors, last_error FROM error_verdicts ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ErrorVerdict{}
	for rows.Next() {
		var v ErrorVerdict
		if err := rows.Scan(&v.ID, &v.At, &v.Provider, &v.Signature, &v.Action, &v.Applied, &v.Verified, &v.Detail, &v.Cause,
			&v.Reason, &v.Note, &v.By, &v.Errors, &v.LastError); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
