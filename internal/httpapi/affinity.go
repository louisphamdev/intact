package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// Anthropic keeps a prompt cache per organization, so a session that moves to
// another account pays the full cache write again. affinity remembers which
// account last answered each session and sends the session back there.
type affinity struct {
	mu sync.Mutex
	m  map[string]affinityEntry
}

type affinityEntry struct {
	conn string
	seen time.Time
}

const (
	affinityTTL = time.Hour // a session idle this long has lost its cache anyway
	affinityMax = 20000     // entries; the oldest are dropped past this
)

// sessionKeyCtx carries the caller's session key from v1 to failover.
type sessionKeyCtx struct{}

func (f *affinity) get(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[key]
	if !ok {
		return ""
	}
	if time.Since(e.seen) > affinityTTL {
		delete(f.m, key)
		return ""
	}
	return e.conn
}

func (f *affinity) set(key, conn string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]affinityEntry{}
	}
	if _, known := f.m[key]; !known && len(f.m) >= affinityMax {
		f.prune()
	}
	f.m[key] = affinityEntry{conn: conn, seen: time.Now()}
}

// prune drops expired entries, then the oldest half if still full.
func (f *affinity) prune() {
	var oldest time.Time
	for k, e := range f.m {
		if time.Since(e.seen) > affinityTTL {
			delete(f.m, k)
		} else if oldest.IsZero() || e.seen.Before(oldest) {
			oldest = e.seen
		}
	}
	if len(f.m) < affinityMax {
		return
	}
	mid := oldest.Add(time.Since(oldest) / 2)
	for k, e := range f.m {
		if !e.seen.After(mid) {
			delete(f.m, k)
		}
	}
}

// sessionKey names the caller's conversation: Claude Code's session header, or
// the session_id inside metadata.user_id (a JSON string in current versions,
// a plain string in older ones). Empty when the request names no session.
func sessionKey(r *http.Request, body []byte) string {
	return boundKey(principalOf(r).keyID, rawSessionKey(r, body))
}

// boundKey scopes a session to the caller's key, so one key cannot move
// another key's sessions, and hashes a long id: the map holds short keys only.
func boundKey(keyID, k string) string {
	if k == "" {
		return ""
	}
	k = keyID + "|" + k
	if len(k) > 64 {
		sum := sha256.Sum256([]byte(k))
		return "h:" + hex.EncodeToString(sum[:])
	}
	return k
}

func rawSessionKey(r *http.Request, body []byte) string {
	if s := r.Header.Get("X-Claude-Code-Session-Id"); s != "" {
		return "s:" + s
	}
	var b struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &b) != nil || b.Metadata.UserID == "" {
		return ""
	}
	var uid struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(b.Metadata.UserID), &uid) == nil && uid.SessionID != "" {
		return "s:" + uid.SessionID
	}
	return "u:" + b.Metadata.UserID
}

// sessionOrder puts the session's account first, then the provider's rotation
// order with standby accounts last, without moving the rotation cursor. It
// declines when the session is unknown, its account is gone, or its account is
// a standby while a normal account is active: then the rotation decides.
func (a *api) sessionOrder(key string, targets []store.Connection) ([]store.Connection, bool) {
	if key == "" {
		return nil, false
	}
	id := a.sessions.get(key)
	var home *store.Connection
	for i := range targets {
		if targets[i].ID == id {
			home = &targets[i]
		}
	}
	if id == "" || home == nil {
		return nil, false
	}
	var normal, standby []store.Connection
	for _, c := range a.rotation(targets[0].Provider).ordered(targets) {
		switch {
		case c.ID == id:
		case c.Standby:
			standby = append(standby, c)
		default:
			normal = append(normal, c)
		}
	}
	if home.Standby && len(normal) > 0 {
		return nil, false
	}
	out := append([]store.Connection{*home}, normal...)
	return append(out, standby...), true
}

// spentUntil is when an account's quota for a model comes back, read from the
// quota cache only (no fetch on the request path). Zero when it is not spent,
// or when no reset time is known: an account is never parked for good.
func (a *api) spentUntil(connID, model string) time.Time {
	a.quota.mu.Lock()
	q, ok := a.quota.m[connID]
	a.quota.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	left, ws := quotaClass(q, model)
	if left < 0 || left > 0 {
		return time.Time{}
	}
	var until time.Time
	for _, w := range ws {
		if t, err := time.Parse(time.RFC3339, w.ResetAt); err == nil && w.UsedPct >= 100 && t.After(until) {
			until = t
		}
	}
	if !until.After(time.Now()) {
		return time.Time{}
	}
	return until
}

// skipSpent drops the attempts whose account has spent its quota for the
// model until the reset. With every account spent it keeps them all, so the
// caller gets the provider's own answer.
func (a *api) skipSpent(attempts []attempt, model string) []attempt {
	kept := make([]attempt, 0, len(attempts))
	for _, at := range attempts {
		m := model
		if at.model != "" {
			m = at.model
		}
		if a.spentUntil(at.conn.ID, m).IsZero() {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		return attempts
	}
	return kept
}
