package httpapi

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateHeaders remembers the rate-limit and quota headers of the last answer
// each account got, so a provider without a quota endpoint (Groq, OpenRouter,
// NVIDIA…) still shows where it stands. Codex and Copilot also report their
// windows this way.
type rateHeaders struct {
	mu sync.Mutex
	m  map[string]rateSnapshot
}

type rateSnapshot struct {
	At      time.Time         `json:"at"`
	Headers map[string]string `json:"headers"`
}

var ratePrefixes = []string{"x-ratelimit-", "ratelimit-", "anthropic-ratelimit-", "x-codex-", "x-quota-snapshot-", "retry-after"}

// capture keeps the quota headers of one answer, if it has any.
func (r *rateHeaders) capture(connID string, h http.Header) {
	got := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		for _, p := range ratePrefixes {
			if strings.HasPrefix(lk, p) && len(v) > 0 {
				got[lk] = v[0]
				break
			}
		}
	}
	if len(got) == 0 {
		return
	}
	r.mu.Lock()
	r.m[connID] = rateSnapshot{At: time.Now().UTC(), Headers: got}
	r.mu.Unlock()
}

func (r *rateHeaders) get(connID string) (rateSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.m[connID]
	return s, ok
}
