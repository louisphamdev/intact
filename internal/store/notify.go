package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// NotifyChannel is a place intact sends alerts to: a Telegram chat (or a
// topic of a forum group), or a webhook. Config holds the type's fields,
// secrets included; the API masks them.
type NotifyChannel struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Type    string            `json:"type"` // telegram, webhook
	Enabled bool              `json:"enabled"`
	Events  []string          `json:"events"` // empty: every event
	Config  map[string]string `json:"config"`
	Created string            `json:"created"`
	LastAt  string            `json:"lastAt"`
	LastErr string            `json:"lastError"`
}

const notifySchema = `
CREATE TABLE IF NOT EXISTS notify_channels (
	id       TEXT PRIMARY KEY,
	name     TEXT NOT NULL,
	type     TEXT NOT NULL,
	enabled  INTEGER NOT NULL DEFAULT 1,
	events   TEXT NOT NULL DEFAULT '[]',
	config   TEXT NOT NULL DEFAULT '{}',
	created  TEXT NOT NULL,
	last_at  TEXT NOT NULL DEFAULT '',
	last_err TEXT NOT NULL DEFAULT ''
);`

// ListNotifyChannels returns every channel, oldest first.
func (s *Store) ListNotifyChannels() ([]NotifyChannel, error) {
	rows, err := s.DB.Query(`SELECT id, name, type, enabled, events, config, created, last_at, last_err FROM notify_channels ORDER BY created, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotifyChannel{}
	for rows.Next() {
		var c NotifyChannel
		var ev, cfg string
		if err := rows.Scan(&c.ID, &c.Name, &c.Type, &c.Enabled, &ev, &cfg, &c.Created, &c.LastAt, &c.LastErr); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(ev), &c.Events)
		json.Unmarshal([]byte(cfg), &c.Config)
		if c.Events == nil {
			c.Events = []string{}
		}
		if c.Config == nil {
			c.Config = map[string]string{}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveNotifyChannel creates a channel (empty id) or replaces one.
func (s *Store) SaveNotifyChannel(c NotifyChannel) (NotifyChannel, error) {
	if c.ID == "" {
		b := make([]byte, 6)
		rand.Read(b)
		c.ID = hex.EncodeToString(b)
		c.Created = time.Now().UTC().Format(time.RFC3339)
	}
	if c.Events == nil {
		c.Events = []string{}
	}
	ev, _ := json.Marshal(c.Events)
	cfg, _ := json.Marshal(c.Config)
	_, err := s.DB.Exec(`INSERT INTO notify_channels (id, name, type, enabled, events, config, created) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, type=excluded.type, enabled=excluded.enabled, events=excluded.events,
		config=excluded.config`, c.ID, c.Name, c.Type, c.Enabled, string(ev), string(cfg), c.Created)
	return c, err
}

// DeleteNotifyChannel removes a channel.
func (s *Store) DeleteNotifyChannel(id string) error {
	_, err := s.DB.Exec(`DELETE FROM notify_channels WHERE id = ?`, id)
	return err
}

// NoteNotifySent records the outcome of the last send to a channel.
func (s *Store) NoteNotifySent(id, errText string) {
	s.DB.Exec(`UPDATE notify_channels SET last_at = ?, last_err = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), errText, id)
}
