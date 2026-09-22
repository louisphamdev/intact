package store

import (
	"fmt"
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
	acked     INTEGER NOT NULL DEFAULT 0
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
	frows, err := s.DB.Query(`SELECT key, path, type, seen, first_obs, last_obs, gone, last_at FROM shape_fields`)
	if err != nil {
		return nil, nil, fmt.Errorf("query shape fields: %w", err)
	}
	defer frows.Close()
	var fields []ShapeField
	for frows.Next() {
		var f ShapeField
		var gone int
		if err := frows.Scan(&f.Key, &f.Path, &f.Type, &f.Seen, &f.FirstObs, &f.LastObs, &gone, &f.LastAt); err != nil {
			return nil, nil, err
		}
		f.Gone = gone != 0
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
		gone := 0
		if f.Gone {
			gone = 1
		}
		if _, err := tx.Exec(`INSERT INTO shape_fields (key, path, type, seen, first_obs, last_obs, gone, last_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(key, path) DO UPDATE SET type = excluded.type, seen = excluded.seen,
			  last_obs = excluded.last_obs, gone = excluded.gone, last_at = excluded.last_at`,
			f.Key, f.Path, f.Type, f.Seen, f.FirstObs, f.LastObs, gone, f.LastAt); err != nil {
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
	_, err := s.DB.Exec(`INSERT INTO shape_changes (at, direction, provider, endpoint, event, path, kind, old_type, new_type, sample)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.At, c.Direction, c.Provider, c.Endpoint, c.Event, c.Path, c.Kind, c.OldType, c.NewType, c.Sample)
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
	q := `SELECT id, at, direction, provider, endpoint, event, path, kind, old_type, new_type, sample, acked FROM shape_changes WHERE id > ?`
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
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query shape changes: %w", err)
	}
	defer rows.Close()
	out := []ShapeChange{}
	for rows.Next() {
		var c ShapeChange
		var acked int
		if err := rows.Scan(&c.ID, &c.At, &c.Direction, &c.Provider, &c.Endpoint, &c.Event, &c.Path, &c.Kind, &c.OldType, &c.NewType, &c.Sample, &acked); err != nil {
			return nil, err
		}
		c.Acked = acked != 0
		out = append(out, c)
	}
	return out, rows.Err()
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
