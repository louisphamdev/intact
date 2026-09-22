package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PendingLogin is an OAuth sign-in waiting for its code. It is kept in the
// database so a restart between opening the sign-in page and pasting the code
// does not lose the PKCE verifier.
type PendingLogin struct {
	State    string
	Provider string
	Verifier string
	Label    string
	Created  time.Time
}

// PendingLoginTTL is how long a sign-in may take.
const PendingLoginTTL = 30 * time.Minute

const pendingSchema = `
CREATE TABLE IF NOT EXISTS oauth_pending (
	state      TEXT PRIMARY KEY,
	provider   TEXT NOT NULL,
	verifier   TEXT NOT NULL,
	label      TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);`

// PutPendingLogin stores a sign-in and drops the expired ones.
func (s *Store) PutPendingLogin(p PendingLogin) error {
	cut := time.Now().Add(-PendingLoginTTL).UTC().Format(time.RFC3339)
	s.DB.Exec(`DELETE FROM oauth_pending WHERE created_at < ?`, cut)
	_, err := s.DB.Exec(`INSERT INTO oauth_pending (state, provider, verifier, label, created_at) VALUES (?, ?, ?, ?, ?)`,
		p.State, p.Provider, p.Verifier, p.Label, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("store pending login: %w", err)
	}
	return nil
}

// TakePendingLogin returns a sign-in once and forgets it; an expired one is
// reported as missing.
func (s *Store) TakePendingLogin(state string) (PendingLogin, bool) {
	var p PendingLogin
	var created string
	err := s.DB.QueryRow(`SELECT state, provider, verifier, label, created_at FROM oauth_pending WHERE state = ?`, state).
		Scan(&p.State, &p.Provider, &p.Verifier, &p.Label, &created)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return PendingLogin{}, false
	}
	s.DB.Exec(`DELETE FROM oauth_pending WHERE state = ?`, state)
	p.Created, _ = time.Parse(time.RFC3339, created)
	if time.Since(p.Created) > PendingLoginTTL {
		return PendingLogin{}, false
	}
	return p, true
}
