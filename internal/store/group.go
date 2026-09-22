package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// A group pools connections, of one provider or several, behind one name so a
// caller can reach them all at /g/<name>/… and let intact pick and fail over.
//
// A member may carry a model. When it does, intact replaces the top-level
// "model" of the request body for that member only, because the same model has
// a different id at each provider (llama-3.3-70b-versatile at groq,
// meta/llama-3.3-70b-instruct at nvidia). A member with no model forwards the
// body untouched, which keeps the passthrough contract for a one-provider group.

// Strategies a group can use.
const (
	// StrategyRoundRobin rotates the first member tried across calls.
	StrategyRoundRobin = "round-robin"
	// StrategyFallback always tries the members in order, so the first one takes
	// the load and the rest only serve when it is busy.
	StrategyFallback = "fallback"
)

// ErrGroupNotFound reports that no group has the requested name.
var ErrGroupNotFound = errors.New("group not found")

// groupName is the shape of a name, which appears in the URL path.
var groupName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidGroupName reports whether name can be used as a group name.
func ValidGroupName(name string) bool { return groupName.MatchString(name) }

// ValidStrategy reports whether s is a known strategy.
func ValidStrategy(s string) bool { return s == StrategyRoundRobin || s == StrategyFallback }

// Group is one pool and its members in the order they are tried.
type Group struct {
	Name     string   `json:"name"`
	Strategy string   `json:"strategy"`
	Members  []Member `json:"members"`
}

// Member is one connection inside a group.
type Member struct {
	ConnectionID string `json:"connectionId"`
	Model        string `json:"model"`
}

const groupSchema = `
CREATE TABLE IF NOT EXISTS groups (
	name       TEXT PRIMARY KEY,
	strategy   TEXT NOT NULL DEFAULT 'round-robin',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS group_members (
	group_name    TEXT NOT NULL,
	connection_id TEXT NOT NULL,
	position      INTEGER NOT NULL,
	model         TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (group_name, connection_id)
);`

// SaveGroup creates a group or replaces the strategy and members of an existing
// one. The members are written in one transaction, so a reader never sees half
// of an edit.
func (s *Store) SaveGroup(g Group) error {
	if !ValidGroupName(g.Name) {
		return fmt.Errorf("invalid group name %q", g.Name)
	}
	if !ValidStrategy(g.Strategy) {
		return fmt.Errorf("invalid strategy %q", g.Strategy)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(
		`INSERT INTO groups (name, strategy, created_at, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET strategy = excluded.strategy, updated_at = excluded.updated_at`,
		g.Name, g.Strategy, now, now); err != nil {
		return fmt.Errorf("upsert group: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM group_members WHERE group_name = ?`, g.Name); err != nil {
		return fmt.Errorf("clear members: %w", err)
	}
	seen := map[string]bool{}
	for i, m := range g.Members {
		if seen[m.ConnectionID] {
			continue
		}
		seen[m.ConnectionID] = true
		var one int
		err := tx.QueryRow(`SELECT 1 FROM connections WHERE id = ?`, m.ConnectionID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("member %s: %w", m.ConnectionID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("check member: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO group_members (group_name, connection_id, position, model) VALUES (?, ?, ?, ?)`,
			g.Name, m.ConnectionID, i, m.Model); err != nil {
			return fmt.Errorf("insert member: %w", err)
		}
	}
	return tx.Commit()
}

// ListGroups returns every group by name, each with its members in order.
func (s *Store) ListGroups() ([]Group, error) {
	rows, err := s.DB.Query(`SELECT name, strategy FROM groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query groups: %w", err)
	}
	out := []Group{}
	idx := map[string]int{}
	for rows.Next() {
		g := Group{Members: []Member{}}
		if err := rows.Scan(&g.Name, &g.Strategy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan group: %w", err)
		}
		idx[g.Name] = len(out)
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	mrows, err := s.DB.Query(
		`SELECT group_name, connection_id, model FROM group_members ORDER BY group_name, position`)
	if err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var name string
		var m Member
		if err := mrows.Scan(&name, &m.ConnectionID, &m.Model); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		if i, ok := idx[name]; ok {
			out[i].Members = append(out[i].Members, m)
		}
	}
	return out, mrows.Err()
}

// GetGroup returns one group with its members in order.
func (s *Store) GetGroup(name string) (Group, error) {
	all, err := s.ListGroups()
	if err != nil {
		return Group{}, err
	}
	for _, g := range all {
		if g.Name == name {
			return g, nil
		}
	}
	return Group{}, ErrGroupNotFound
}

// DeleteGroup removes a group and its memberships. The connections stay.
func (s *Store) DeleteGroup(name string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM groups WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrGroupNotFound
	}
	if _, err := tx.Exec(`DELETE FROM group_members WHERE group_name = ?`, name); err != nil {
		return fmt.Errorf("delete members: %w", err)
	}
	return tx.Commit()
}
