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
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Name     string `json:"name"`
	// Members are connection ids, in the order they were added. A member that
	// is gone or switched off just does not serve.
	Members []string `json:"members"`
	// Models are the model ids the profile serves, when it has its own list;
	// nil serves everything its provider does. A listed model's level
	// variants ("-high", …) are served with it.
	Models    []string `json:"models,omitempty"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
}

// ProfileUpdate carries the fields a caller wants to change; a nil field
// keeps the stored value.
type ProfileUpdate struct {
	Name    *string
	Members *[]string
	Models  *[]string
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
	models     TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE (provider, name)
);`

// migrateProfiles adds the columns a running install's table is missing; a
// fresh database gets them from the schema.
func (s *Store) migrateProfiles() error {
	rows, err := s.DB.Query(`PRAGMA table_info(profiles)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !have["models"] {
		if _, err := s.DB.Exec(`ALTER TABLE profiles ADD COLUMN models TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("alter profiles add models: %w", err)
		}
	}
	return nil
}

func scanProfile(scan func(dest ...any) error) (Profile, error) {
	var p Profile
	var members, models string
	if err := scan(&p.ID, &p.Provider, &p.Name, &members, &models, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Profile{}, err
	}
	json.Unmarshal([]byte(members), &p.Members)
	if p.Members == nil {
		p.Members = []string{}
	}
	// An empty column is "no list of its own": the profile serves everything
	// its provider does.
	json.Unmarshal([]byte(models), &p.Models)
	return p, nil
}

// CreateProfile stores a profile and returns it. A nil or empty models list
// means "serve everything the provider does".
func (s *Store) CreateProfile(providerID, name string, members, models []string) (Profile, error) {
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
	mraw, err := encodeProfileModels(models)
	if err != nil {
		return Profile{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.DB.Exec(
		`INSERT INTO profiles (id, provider, name, members, models, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, providerID, name, string(raw), mraw, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Profile{}, ErrProfileExists
		}
		return Profile{}, fmt.Errorf("insert profile: %w", err)
	}
	p := Profile{ID: id, Provider: providerID, Name: name, Members: members,
		CreatedAt: now, UpdatedAt: now}
	if mraw != "" {
		p.Models = models
	}
	return p, nil
}

// encodeProfileModels turns a profile's model list into its column value: an
// empty or nil list stores ” (no list of its own).
func encodeProfileModels(models []string) (string, error) {
	if len(models) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(models)
	if err != nil {
		return "", fmt.Errorf("encode models: %w", err)
	}
	return string(raw), nil
}

// ListProfiles returns every profile, oldest first, so a caller that looks one
// up by route sees a stable answer.
func (s *Store) ListProfiles() ([]Profile, error) {
	rows, err := s.DB.Query(
		`SELECT id, provider, name, members, models, created_at, updated_at FROM profiles ORDER BY created_at ASC`)
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

// UpdateProfile changes a profile's name, members, models or any of them; a
// nil field keeps its value. An empty (non-nil) members or models list sets
// that empty value, which for models means "no list of its own". It reports
// ErrNotFound when no row matched.
func (s *Store) UpdateProfile(id string, ch ProfileUpdate) (Profile, error) {
	p, err := s.Profile(id)
	if err != nil {
		return Profile{}, err
	}
	if ch.Name != nil && *ch.Name != "" {
		p.Name = *ch.Name
	}
	if ch.Members != nil {
		p.Members = *ch.Members
	}
	if ch.Models != nil {
		p.Models = *ch.Models
	}
	raw, err := json.Marshal(p.Members)
	if err != nil {
		return Profile{}, fmt.Errorf("encode members: %w", err)
	}
	mraw, err := encodeProfileModels(p.Models)
	if err != nil {
		return Profile{}, err
	}
	p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`UPDATE profiles SET name = ?, members = ?, models = ?, updated_at = ? WHERE id = ?`,
		p.Name, string(raw), mraw, p.UpdatedAt, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Profile{}, ErrProfileExists
		}
		return Profile{}, fmt.Errorf("update profile: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Profile{}, ErrNotFound
	}
	if mraw == "" {
		p.Models = nil
	}
	return p, nil
}

// Profile returns one profile. It reports ErrNotFound when none has the id.
func (s *Store) Profile(id string) (Profile, error) {
	row := s.DB.QueryRow(
		`SELECT id, provider, name, members, models, created_at, updated_at FROM profiles WHERE id = ?`, id)
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
