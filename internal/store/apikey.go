package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// APIKey is a token a client sends to /v1, /api and /mcp. Key is filled only
// when the full value is asked for; a listing carries the masked form.
type APIKey struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Key     string `json:"key,omitempty"`
	Masked  string `json:"masked"`
	Enabled bool   `json:"enabled"`
	Trusted bool   `json:"trusted"`
	// Models lists the "<provider>/<model>" ids the key may call. Empty means
	// every model.
	Models []string `json:"models"`
	// ExpiresAt (RFC 3339, UTC) ends the key; empty means it never expires.
	ExpiresAt string `json:"expiresAt"`
	// RPM caps the requests the key makes in any 60 seconds; 0 means no cap.
	RPM       int    `json:"rpm"`
	LastUsed  string `json:"lastUsed"`
	CreatedAt string `json:"createdAt"`
}

// Expired reports whether the key's expiry has passed at now.
func (k APIKey) Expired(now time.Time) bool {
	if k.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, k.ExpiresAt)
	return err != nil || !now.Before(t)
}

// KeyUsageRow is one day's total for an API key and model.
type KeyUsageRow struct {
	Day          string `json:"day"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	CachedTokens int64  `json:"cachedTokens"`
	Requests     int64  `json:"requests"`
}

// ErrKeyNotFound reports that no API key has the requested id.
var ErrKeyNotFound = errors.New("api key not found")

const apiKeySchema = `
CREATE TABLE IF NOT EXISTS api_keys (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	key        TEXT NOT NULL UNIQUE,
	enabled    INTEGER NOT NULL DEFAULT 1,
	trusted    INTEGER NOT NULL DEFAULT 0,
	models     TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_key_daily (
	day           TEXT NOT NULL,
	key_id        TEXT NOT NULL,
	model         TEXT NOT NULL,
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	requests      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, key_id, model)
);`

func mask(k string) string {
	if len(k) < 16 {
		return "••••"
	}
	return k[:10] + "••••••••" + k[len(k)-4:]
}

func (s *Store) migrateAPIKeys() error {
	rows, err := s.DB.Query(`PRAGMA table_info(api_keys)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, col := range []struct{ name, def string }{
		{"trusted", "INTEGER NOT NULL DEFAULT 0"},
		{"models", "TEXT NOT NULL DEFAULT ''"},
		{"expires_at", "TEXT NOT NULL DEFAULT ''"},
		{"rpm", "INTEGER NOT NULL DEFAULT 0"},
		{"last_used", "TEXT NOT NULL DEFAULT ''"},
	} {
		if have[col.name] {
			continue
		}
		if _, err := s.DB.Exec(`ALTER TABLE api_keys ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
			return fmt.Errorf("alter api_keys add %s: %w", col.name, err)
		}
	}
	return nil
}

// encodeModels stores a model list; an empty list is stored as "" (every model).
func encodeModels(models []string) string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return ""
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// parseModels reads a stored model list. "" and a list that does not parse
// both mean every model: the column is written only by encodeModels.
func parseModels(v string) []string {
	var out []string
	if v != "" {
		json.Unmarshal([]byte(v), &out)
	}
	return out
}

// CreateAPIKey makes a new random key and returns it in full. An empty model
// list lets the key call every model.
func (s *Store) CreateAPIKey(name string, models []string) (APIKey, error) {
	id, err := newID()
	if err != nil {
		return APIKey{}, err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return APIKey{}, err
	}
	enc := encodeModels(models)
	k := APIKey{ID: id, Name: name, Key: "sk-intact-" + hex.EncodeToString(b), Enabled: true, Trusted: false,
		Models: parseModels(enc), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	k.Masked = mask(k.Key)
	if _, err := s.DB.Exec(`INSERT INTO api_keys (id, name, key, enabled, trusted, models, created_at) VALUES (?, ?, ?, 1, 0, ?, ?)`,
		k.ID, k.Name, k.Key, enc, k.CreatedAt); err != nil {
		return APIKey{}, fmt.Errorf("insert api key: %w", err)
	}
	return k, nil
}

// ListAPIKeys returns every key, newest first, masked.
func (s *Store) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.DB.Query(`SELECT id, name, key, enabled, trusted, models, expires_at, rpm, last_used, created_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query api keys: %w", err)
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		var full, models string
		var en, tr int
		if err := rows.Scan(&k.ID, &k.Name, &full, &en, &tr, &models, &k.ExpiresAt, &k.RPM, &k.LastUsed, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		k.Masked, k.Enabled, k.Trusted, k.Models = mask(full), en != 0, tr != 0, parseModels(models)
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevealAPIKey returns one key in full.
func (s *Store) RevealAPIKey(id string) (string, error) {
	var k string
	err := s.DB.QueryRow(`SELECT key FROM api_keys WHERE id = ?`, id).Scan(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrKeyNotFound
	}
	return k, err
}

// SetAPIKeyEnabled turns a key on or off.
func (s *Store) SetAPIKeyEnabled(id string, on bool) error {
	v := 0
	if on {
		v = 1
	}
	res, err := s.DB.Exec(`UPDATE api_keys SET enabled = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("set api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// SetAPIKeyModels replaces the models a key may call; an empty list allows all.
func (s *Store) SetAPIKeyModels(id string, models []string) error {
	res, err := s.DB.Exec(`UPDATE api_keys SET models = ? WHERE id = ?`, encodeModels(models), id)
	if err != nil {
		return fmt.Errorf("set api key models: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// DeleteAPIKey removes a key and its usage; a client using it is refused from
// then on.
func (s *Store) DeleteAPIKey(id string) error {
	s.DB.Exec(`DELETE FROM usage_key_daily WHERE key_id = ?`, id)
	res, err := s.DB.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// ValidAPIKey reports whether k is an enabled key.
func (s *Store) ValidAPIKey(k string) bool {
	if k == "" {
		return false
	}
	var one int
	return s.DB.QueryRow(`SELECT 1 FROM api_keys WHERE key = ? AND enabled = 1`, k).Scan(&one) == nil
}

// SetAPIKeyTrusted turns the trusted flag of a key on or off.
// When revoked, open traces of this key are moved to status 'revoked'.
func (s *Store) SetAPIKeyTrusted(id string, trusted bool) error {
	v := 0
	if trusted {
		v = 1
	}
	res, err := s.DB.Exec(`UPDATE api_keys SET trusted = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("set api key trusted: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	if !trusted {
		_, _ = s.DB.Exec(`UPDATE contract_traces SET status = 'revoked' WHERE key_id = ? AND status = 'open'`, id)
	}
	return nil
}

// IsKeyTrusted reports whether an enabled key is trusted.
func (s *Store) IsKeyTrusted(id string) bool {
	if id == "" {
		return false
	}
	var tr int
	if s.DB.QueryRow(`SELECT trusted FROM api_keys WHERE id = ? AND enabled = 1`, id).Scan(&tr) != nil {
		return false
	}
	return tr != 0
}

// APIKeyInfoByToken returns id, name, and trusted flag for an enabled key.
func (s *Store) APIKeyInfoByToken(tok string) (id, name string, trusted bool, ok bool) {
	if tok == "" {
		return "", "", false, false
	}
	var tr int
	if s.DB.QueryRow(`SELECT id, name, trusted FROM api_keys WHERE key = ? AND enabled = 1`, tok).Scan(&id, &name, &tr) != nil {
		return "", "", false, false
	}
	return id, name, tr != 0, true
}

// APIKeyByToken returns an enabled key with its model list and limits, without
// the key itself. The id is the caller's identity: a name is free text that the
// operator can repeat.
func (s *Store) APIKeyByToken(tok string) (APIKey, bool) {
	if tok == "" {
		return APIKey{}, false
	}
	var k APIKey
	var enc string
	if s.DB.QueryRow(`SELECT id, name, models, expires_at, rpm FROM api_keys WHERE key = ? AND enabled = 1`, tok).
		Scan(&k.ID, &k.Name, &enc, &k.ExpiresAt, &k.RPM) != nil {
		return APIKey{}, false
	}
	k.Enabled, k.Models = true, parseModels(enc)
	return k, true
}

// SetAPIKeyLimits stores a key's expiry (RFC 3339, or "" for none) and its
// requests per minute (0 for no cap).
func (s *Store) SetAPIKeyLimits(id, expiresAt string, rpm int) error {
	res, err := s.DB.Exec(`UPDATE api_keys SET expires_at = ?, rpm = ? WHERE id = ?`, expiresAt, rpm, id)
	if err != nil {
		return fmt.Errorf("set api key limits: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// AddKeyUsage folds one request's token counts into the key's daily counter and
// records when the key was last used.
func (s *Store) AddKeyUsage(day, keyID, model string, inputTokens, outputTokens, cachedTokens int64) error {
	if _, err := s.DB.Exec(
		`INSERT INTO usage_key_daily (day, key_id, model, input_tokens, output_tokens, cached_tokens, requests)
		 VALUES (?, ?, ?, ?, ?, ?, 1)
		 ON CONFLICT(day, key_id, model) DO UPDATE SET
		   input_tokens  = input_tokens  + excluded.input_tokens,
		   output_tokens = output_tokens + excluded.output_tokens,
		   cached_tokens = cached_tokens + excluded.cached_tokens,
		   requests      = requests + 1`,
		day, keyID, model, inputTokens, outputTokens, cachedTokens); err != nil {
		return fmt.Errorf("add key usage: %w", err)
	}
	_, err := s.DB.Exec(`UPDATE api_keys SET last_used = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), keyID)
	return err
}

// KeyUsage returns a key's daily counters from day since on, newest day first.
func (s *Store) KeyUsage(keyID, since string) ([]KeyUsageRow, error) {
	rows, err := s.DB.Query(
		`SELECT day, model, input_tokens, output_tokens, cached_tokens, requests FROM usage_key_daily
		 WHERE key_id = ? AND day >= ? ORDER BY day DESC, model`, keyID, since)
	if err != nil {
		return nil, fmt.Errorf("query key usage: %w", err)
	}
	defer rows.Close()
	out := []KeyUsageRow{}
	for rows.Next() {
		var r KeyUsageRow
		if err := rows.Scan(&r.Day, &r.Model, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.Requests); err != nil {
			return nil, fmt.Errorf("scan key usage: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
