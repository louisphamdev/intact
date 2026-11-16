package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Profile is a named group of one provider's accounts. A call that names the
// profile ("<provider>-<name>/<model>") is served by the group's members
// alone; every other call of the provider keeps using all of its accounts.
type Profile struct {
	ID        string   `json:"id"`
	Provider  string   `json:"provider"`
	Name      string   `json:"name"`
	// Members are connection ids, in the order they were added. A member that
	// is gone or switched off just does not serve.
	Members   []string `json:"members"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
}

// ErrProfileExists reports that the provider already has a profile of that
// name.
var ErrProfileExists = errors.New("profile already exists")

const profilesSchema = `
CREATE TABLE IF NOT EXISTS profiles (
	id         TEXT PRIMARY KEY,
	provider   TEXT NOT NULL,
	name       TEXT NOT NULL,
	members    TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (provider, name)
);`

func scanProfile(scan func(dest ...any) error) (Profile, error) {
	var p Profile
	var members string
	if err := scan(&p.ID, &p.Provider, &p.Name, &members, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Profile{}, err
	}
	json.Unmarshal([]byte(members), &p.Members)
	if p.Members == nil {
		p.Members = []string{}
	}
	return p, nil
}

// CreateProfile stores a profile and returns it.
func (s *Store) CreateProfile(providerID, name string, members []string) (Profile, error) {
	if members == nil {
		members = []string{}
	}
	id, err := newID()
	if err != nil {
		return Profile{}, fmt.Errorf("generate id: %w", err)
	}
	raw, err := json.Marshal(members)
	if err != nil {
		return Profile{}, fmt.Errorf("encode members: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.DB.Exec(
		`INSERT INTO profiles (id, provider, name, members, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, providerID, name, string(raw), now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Profile{}, ErrProfileExists
		}
		return Profile{}, fmt.Errorf("insert profile: %w", err)
	}
	return Profile{ID: id, Provider: providerID, Name: name, Members: members,
		CreatedAt: now, UpdatedAt: now}, nil
}

// ListProfiles returns every profile, oldest first, so a caller that looks one
// up by route sees a stable answer.
func (s *Store) ListProfiles() ([]Profile, error) {
	rows, err := s.DB.Query(
		`SELECT id, provider, name, members, created_at, updated_at FROM profiles ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("query profiles: %w", err)
	}
	defer rows.Close()

	out := []Profile{}
	for rows.Next() {
		p, err := scanProfile(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateProfile changes a profile's name, members or both; an empty name or
// nil members keeps its value. It reports ErrNotFound when no row matched.
func (s *Store) UpdateProfile(id, name string, members []string) (Profile, error) {
	p, err := s.Profile(id)
	if err != nil {
		return Profile{}, err
	}
	if name != "" {
		p.Name = name
	}
	if members != nil {
		p.Members = members
	}
	raw, err := json.Marshal(p.Members)
	if err != nil {
		return Profile{}, fmt.Errorf("encode members: %w", err)
	}
	p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`UPDATE profiles SET name = ?, members = ?, updated_at = ? WHERE id = ?`,
		p.Name, string(raw), p.UpdatedAt, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Profile{}, ErrProfileExists
		}
		return Profile{}, fmt.Errorf("update profile: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Profile{}, ErrNotFound
	}
	return p, nil
}

// Profile returns one profile. It reports ErrNotFound when none has the id.
func (s *Store) Profile(id string) (Profile, error) {
	row := s.DB.QueryRow(
		`SELECT id, provider, name, members, created_at, updated_at FROM profiles WHERE id = ?`, id)
	p, err := scanProfile(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("read profile: %w", err)
	}
	return p, nil
}

// DeleteProfile removes one profile. It reports ErrNotFound when no row
// matched, so a caller can tell a real delete from a no-op.
func (s *Store) DeleteProfile(id string) error {
	res, err := s.DB.Exec(`DELETE FROM profiles WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
