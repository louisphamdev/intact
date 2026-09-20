package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound reports that no connection has the requested id.
var ErrNotFound = errors.New("connection not found")

// Connection describes an account without its credential. The secret is
// deliberately absent: anything that can reach this type can be logged or
// serialized, and a credential must survive neither.
type Connection struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
	IsActive bool   `json:"isActive"`
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateConnection stores a credential and returns the record without it.
func (s *Store) CreateConnection(provider, label, secret string) (Connection, error) {
	id, err := newID()
	if err != nil {
		return Connection{}, fmt.Errorf("generate id: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.DB.Exec(
		`INSERT INTO connections (id, provider, label, secret, is_active, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 1, ?, ?)`,
		id, provider, label, secret, now, now)
	if err != nil {
		return Connection{}, fmt.Errorf("insert connection: %w", err)
	}
	return Connection{ID: id, Provider: provider, Label: label, IsActive: true}, nil
}

// ListConnections returns every connection, newest first, without secrets.
func (s *Store) ListConnections() ([]Connection, error) {
	rows, err := s.DB.Query(
		`SELECT id, provider, label, is_active FROM connections ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query connections: %w", err)
	}
	defer rows.Close()

	out := []Connection{}
	for rows.Next() {
		var c Connection
		var active int
		if err := rows.Scan(&c.ID, &c.Provider, &c.Label, &active); err != nil {
			return nil, fmt.Errorf("scan connection: %w", err)
		}
		c.IsActive = active != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// Secret returns the credential for one connection. Callers must not log it.
func (s *Store) Secret(id string) (string, error) {
	var secret string
	err := s.DB.QueryRow(`SELECT secret FROM connections WHERE id = ?`, id).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return secret, nil
}
