package httpapi

import (
	"sync"
	"time"
)

// maxKeyRPM bounds a key's cap, and with it the timestamps kept per key.
const maxKeyRPM = 100000

// keyLimiter counts each API key's requests in the last 60 seconds.
type keyLimiter struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

// allow records a request by key at now when the key made fewer than rpm in
// the minute before. Otherwise it records nothing and returns how long until
// the oldest of those leaves the window.
func (l *keyLimiter) allow(key string, rpm int, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.window(key, now)
	if len(ts) >= rpm {
		return false, ts[len(ts)-rpm].Add(time.Minute).Sub(now)
	}
	l.seen[key] = append(ts, now)
	return true, 0
}

// count returns the requests key made in the minute before now.
func (l *keyLimiter) count(key string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.window(key, now))
}

func (l *keyLimiter) window(key string, now time.Time) []time.Time {
	ts := l.seen[key]
	i := 0
	for i < len(ts) && !ts[i].After(now.Add(-time.Minute)) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.seen, key)
		return nil
	}
	l.seen[key] = ts
	return ts
}
