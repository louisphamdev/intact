package store

import (
	"fmt"
	"strings"
	"time"
)

// ShapeField is one field path learned for a structure key.
type ShapeField struct {
	Key      string `json:"key"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	Seen     int64  `json:"seen"`
	FirstObs int64  `json:"firstObs"`
	LastObs  int64  `json:"lastObs"`
	Gone     bool   `json:"gone"`
	LastAt   string `json:"lastAt"`
	// Legacy marks a path the code before the field-name fix learned. Such a
	// path may hold a real field name that was collapsed to "{*}", so the
	// observer maps it instead of reporting it as removed.
	Legacy bool `json:"legacy,omitempty"`
}

// ShapeChange is one recorded change of structure.
type ShapeChange struct {
	ID        int64  `json:"id"`
	At        string `json:"at"`
	Direction string `json:"direction"`
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint"`
	Event     string `json:"event"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	OldType   string `json:"oldType"`
	NewType   string `json:"newType"`
	Sample    string `json:"sample"`
	Acked     bool   `json:"acked"`
	// Client is who sent a request change: the API key's name, "env" for the
	// environment token, "internal" for intact's own calls.
	Client string `json:"client,omitempty"`
	// ClientKeyID is the API key ID that sent the request change.
	ClientKeyID string `json:"clientKeyId,omitempty"`
	// Verdict is the reviewer's category (see the drift review), with its
	// confidence; AutoAcked marks a change the reviewer acknowledged itself.
	Verdict     string  `json:"verdict,omitempty"`
	VerdictConf float64 `json:"verdictConf,omitempty"`
	VerdictAt   string  `json:"verdictAt,omitempty"`
	AutoAcked   bool    `json:"autoAcked,omitempty"`
	// VerdictBy is the model that gave the verdict; VerdictNote its reason
	// (a resolver's) or why the change was kept. Resolved marks a change a
	// resolver has looked at after the decision model was unsure.
	VerdictBy   string `json:"verdictBy,omitempty"`
	VerdictNote string `json:"verdictNote,omitempty"`
	Resolved    bool   `json:"resolved,omitempty"`
}

// Verdict is what a reviewer decided about a change.
type Verdict struct {
	Cause    string
	Conf     float64
	By       string
	Note     string
	Ack      bool
	Resolved bool
}

// migrateDrift adds the columns newer than the table.
func (s *Store) migrateDrift() error {
	for _, col := range []string{"client TEXT NOT NULL DEFAULT ''", "client_key_id TEXT NOT NULL DEFAULT ''", "verdict TEXT NOT NULL DEFAULT ''",
		"verdict_conf REAL NOT NULL DEFAULT 0", "verdict_at TEXT NOT NULL DEFAULT ''", "auto_acked INTEGER NOT NULL DEFAULT 0",
		"verdict_by TEXT NOT NULL DEFAULT ''", "verdict_note TEXT NOT NULL DEFAULT ''", "resolved INTEGER NOT NULL DEFAULT 0"} {
		if _, err := s.DB.Exec("ALTER TABLE shape_changes ADD COLUMN " + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate shape_changes: %w", err)
		}
	}
	return s.migrateShapeFields()
}

// migrateShapeFields adds the legacy column. The ALTER succeeds exactly once,
// on a database the old code wrote, so that is where the already learned "{*}"
// paths are marked. A later start finds the column and does nothing.
func (s *Store) migrateShapeFields() error {
	if _, err := s.DB.Exec("ALTER TABLE shape_fields ADD COLUMN legacy INTEGER NOT NULL DEFAULT 0"); err != nil {
		if strings.Contains(err.Error(), "duplicate column") {
			return nil
		}
		return fmt.Errorf("migrate shape_fields: %w", err)
	}
	if _, err := s.DB.Exec("UPDATE shape_fields SET legacy = 1 WHERE path LIKE '%{*}%'"); err != nil {
		return fmt.Errorf("mark learned shape fields: %w", err)
	}
	return nil
}

// UnreviewedShapeChanges returns the changes no reviewer has judged yet and
// nobody has acknowledged, oldest first.
func (s *Store) UnreviewedShapeChanges(limit int) ([]ShapeChange, error) {
	return s.queryShapeChanges(`SELECT `+shapeChangeCols+` FROM shape_changes WHERE verdict = '' AND acked = 0 ORDER BY id LIMIT ?`, limit)
}

// SetShapeVerdict records a reviewer's verdict, and acknowledges the change
// when v.Ack is set.
func (s *Store) SetShapeVerdict(id int64, v Verdict) error {
	a, r := 0, 0
	if v.Ack {
		a = 1
	}
	if v.Resolved {
		r = 1
	}
	if len(v.Note) > 500 {
		v.Note = v.Note[:500]
	}
	_, err := s.DB.Exec(`UPDATE shape_changes SET verdict = ?, verdict_conf = ?, verdict_at = ?, verdict_by = ?, verdict_note = ?,
		resolved = ?, auto_acked = ?, acked = CASE WHEN ? = 1 THEN 1 ELSE acked END WHERE id = ?`,
		v.Cause, v.Conf, time.Now().UTC().Format(time.RFC3339), v.By, v.Note, r, a, a, id)
	return err
}

// UnresolvedShapeChanges returns the changes a decision model judged but
// left open, that no resolver has settled yet.
func (s *Store) UnresolvedShapeChanges(limit int) ([]ShapeChange, error) {
	return s.queryShapeChanges(`SELECT `+shapeChangeCols+` FROM shape_changes
		WHERE acked = 0 AND verdict != '' AND resolved = 0 ORDER BY id LIMIT ?`, limit)
}

// ShapeChangesSince returns the changes recorded after a time, for a
// reviewer's context.
func (s *Store) ShapeChangesSince(at string) ([]ShapeChange, error) {
	return s.queryShapeChanges(`SELECT `+shapeChangeCols+` FROM shape_changes WHERE at >= ? ORDER BY id`, at)
}

const shapeChangeCols = `id, at, direction, provider, endpoint, event, path, kind, old_type, new_type, sample, acked,
	client, verdict, verdict_conf, verdict_at, auto_acked, verdict_by, verdict_note, resolved, client_key_id`

func (s *Store) queryShapeChanges(q string, args ...any) ([]ShapeChange, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query shape changes: %w", err)
	}
	defer rows.Close()
	out := []ShapeChange{}
	for rows.Next() {
		var c ShapeChange
		var acked, auto, resolved int
		if err := rows.Scan(&c.ID, &c.At, &c.Direction, &c.Provider, &c.Endpoint, &c.Event, &c.Path, &c.Kind,
			&c.OldType, &c.NewType, &c.Sample, &acked, &c.Client, &c.Verdict, &c.VerdictConf, &c.VerdictAt, &auto,
			&c.VerdictBy, &c.VerdictNote, &resolved, &c.ClientKeyID); err != nil {
			return nil, err
		}
		c.Acked, c.AutoAcked, c.Resolved = acked != 0, auto != 0, resolved != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

const driftSchema = `
CREATE TABLE IF NOT EXISTS shape_keys (
	key          TEXT PRIMARY KEY,
	observations INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS shape_fields (
	key       TEXT NOT NULL,
	path      TEXT NOT NULL,
	type      TEXT NOT NULL,
	seen      INTEGER NOT NULL DEFAULT 0,
	first_obs INTEGER NOT NULL,
	last_obs  INTEGER NOT NULL,
	gone      INTEGER NOT NULL DEFAULT 0,
	last_at   TEXT NOT NULL DEFAULT '',
	legacy    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (key, path)
);
CREATE TABLE IF NOT EXISTS shape_changes (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	at        TEXT NOT NULL,
	direction TEXT NOT NULL,
	provider  TEXT NOT NULL,
	endpoint  TEXT NOT NULL,
	event     TEXT NOT NULL DEFAULT '',
	path      TEXT NOT NULL,
	kind      TEXT NOT NULL,
	old_type  TEXT NOT NULL DEFAULT '',
	new_type  TEXT NOT NULL DEFAULT '',
	sample    TEXT NOT NULL DEFAULT '',
	acked     INTEGER NOT NULL DEFAULT 0,
	client_key_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS shape_changes_acked ON shape_changes (acked, id);`

// LoadShapes returns every learned key with its observation count and fields.
func (s *Store) LoadShapes() (map[string]int64, []ShapeField, error) {
	keys := map[string]int64{}
	rows, err := s.DB.Query(`SELECT key, observations FROM shape_keys`)
	if err != nil {
		return nil, nil, fmt.Errorf("query shape keys: %w", err)
	}
	for rows.Next() {
		var k string
		var n int64
		rows.Scan(&k, &n)
		keys[k] = n
	}
	rows.Close()
	frows, err := s.DB.Query(`SELECT key, path, type, seen, first_obs, last_obs, gone, last_at, legacy FROM shape_fields`)
	if err != nil {
		return nil, nil, fmt.Errorf("query shape fields: %w", err)
	}
	defer frows.Close()
	var fields []ShapeField
	for frows.Next() {
		var f ShapeField
		var gone, legacy int
		if err := frows.Scan(&f.Key, &f.Path, &f.Type, &f.Seen, &f.FirstObs, &f.LastObs, &gone, &f.LastAt, &legacy); err != nil {
			return nil, nil, err
		}
		f.Gone, f.Legacy = gone != 0, legacy != 0
		fields = append(fields, f)
	}
	return keys, fields, frows.Err()
}

// SaveShapes writes observation counts and fields in one transaction.
func (s *Store) SaveShapes(keys map[string]int64, fields []ShapeField) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, n := range keys {
		if _, err := tx.Exec(`INSERT INTO shape_keys (key, observations) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET observations = excluded.observations`, k, n); err != nil {
			return fmt.Errorf("save shape key: %w", err)
		}
	}
	for _, f := range fields {
		gone, legacy := 0, 0
		if f.Gone {
			gone = 1
		}
		if f.Legacy {
			legacy = 1
		}
		if _, err := tx.Exec(`INSERT INTO shape_fields (key, path, type, seen, first_obs, last_obs, gone, last_at, legacy)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(key, path) DO UPDATE SET type = excluded.type, seen = excluded.seen,
			  last_obs = excluded.last_obs, gone = excluded.gone, last_at = excluded.last_at, legacy = excluded.legacy`,
			f.Key, f.Path, f.Type, f.Seen, f.FirstObs, f.LastObs, gone, f.LastAt, legacy); err != nil {
			return fmt.Errorf("save shape field: %w", err)
		}
	}
	return tx.Commit()
}

// AddShapeChange records one change.
func (s *Store) AddShapeChange(c ShapeChange) error {
	if c.At == "" {
		c.At = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.DB.Exec(`INSERT INTO shape_changes (at, direction, provider, endpoint, event, path, kind, old_type, new_type, sample, client, client_key_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.At, c.Direction, c.Provider, c.Endpoint, c.Event, c.Path, c.Kind, c.OldType, c.NewType, c.Sample, c.Client, c.ClientKeyID)
	if err != nil {
		return fmt.Errorf("add shape change: %w", err)
	}
	return nil
}

// ShapeChangeFilter narrows ListShapeChanges.
type ShapeChangeFilter struct {
	Provider  string
	Direction string
	Unacked   bool
	SinceID   int64
	Limit     int
}

// ListShapeChanges returns changes, newest first.
func (s *Store) ListShapeChanges(f ShapeChangeFilter) ([]ShapeChange, error) {
	q := `SELECT ` + shapeChangeCols + ` FROM shape_changes WHERE id > ?`
	args := []any{f.SinceID}
	if f.Provider != "" {
		q += ` AND provider = ?`
		args = append(args, f.Provider)
	}
	if f.Direction != "" {
		q += ` AND direction = ?`
		args = append(args, f.Direction)
	}
	if f.Unacked {
		q += ` AND acked = 0`
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit)
	return s.queryShapeChanges(q, args...)
}

// AckShapeChanges marks changes as seen: the ids given, or every change when
// ids is empty.
func (s *Store) AckShapeChanges(ids []int64) (int64, error) {
	if len(ids) == 0 {
		res, err := s.DB.Exec(`UPDATE shape_changes SET acked = 1 WHERE acked = 0`)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	var n int64
	for _, id := range ids {
		res, err := s.DB.Exec(`UPDATE shape_changes SET acked = 1 WHERE id = ?`, id)
		if err != nil {
			return n, err
		}
		k, _ := res.RowsAffected()
		n += k
	}
	return n, nil
}

// CountUnackedShapeChanges returns how many changes wait for a look.
func (s *Store) CountUnackedShapeChanges() int64 {
	var n int64
	s.DB.QueryRow(`SELECT COUNT(*) FROM shape_changes WHERE acked = 0`).Scan(&n)
	return n
}
