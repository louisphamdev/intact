package httpapi

import (
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
	if !ok || time.Since(e.seen) > affinityTTL {
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
	if len(f.m) >= affinityMax {
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
		if e.seen.Before(mid) {
			delete(f.m, k)
		}
	}
}

// sessionKey names the caller's conversation: Claude Code's session header, or
// the session_id inside metadata.user_id (a JSON string in current versions,
// a plain string in older ones). Empty when the request names no session.
func sessionKey(r *http.Request, body []byte) string {
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

// homeOf returns the index of the session's account among targets, or -1.
func (a *api) homeOf(key string, targets []store.Connection) int {
	if key == "" {
		return -1
	}
	id := a.sessions.get(key)
	for i, c := range targets {
		if id != "" && c.ID == id {
			return i
		}
	}
	return -1
}
