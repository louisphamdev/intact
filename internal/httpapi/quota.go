package httpapi

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// QuotaWindow is one limit of an account: a rolling window, a monthly pool,
// a model's share. UsedPct is 0–100, or -1 when the provider gives no ratio.
type QuotaWindow struct {
	Name      string  `json:"name"`
	UsedPct   float64 `json:"usedPct"`
	Used      string  `json:"used,omitempty"`
	Limit     string  `json:"limit,omitempty"`
	ResetAt   string  `json:"resetAt,omitempty"`
	Unlimited bool    `json:"unlimited,omitempty"`
}

// AccountQuota is what intact knows of one account's quota.
type AccountQuota struct {
	ConnectionID string        `json:"connectionId"`
	Provider     string        `json:"provider"`
	Label        string        `json:"label"`
	Plan         string        `json:"plan,omitempty"`
	Source       string        `json:"source"` // "api": read from the provider; "headers": from the last answer
	Windows      []QuotaWindow `json:"windows"`
	Error        string        `json:"error,omitempty"`
	FetchedAt    string        `json:"fetchedAt"`
}

const quotaTTL = time.Minute

type quotaCache struct {
	mu sync.Mutex
	m  map[string]AccountQuota
}

// quotaFetchers read an account's quota from the provider's own endpoint.
var quotaFetchers = map[string]func(a *api, ctx context.Context, c store.Connection, token string) (AccountQuota, error){}

// quotaFor returns an account's quota: from the provider when it has a quota
// endpoint, otherwise from the rate-limit headers of its last answer.
func (a *api) quotaFor(ctx context.Context, c store.Connection, refresh bool) AccountQuota {
	a.quota.mu.Lock()
	q, hit := a.quota.m[c.ID]
	a.quota.mu.Unlock()
	if hit && !refresh {
		if t, err := time.Parse(time.RFC3339, q.FetchedAt); err == nil && time.Since(t) < quotaTTL {
			return q
		}
	}
	q = AccountQuota{ConnectionID: c.ID, Provider: c.Provider, Label: c.Label, Windows: []QuotaWindow{},
		FetchedAt: time.Now().UTC().Format(time.RFC3339)}
	if fetch, ok := quotaFetchers[c.Provider]; ok {
		token, err := a.secretFor(ctx, c.ID)
		if err == nil {
			var got AccountQuota
			if got, err = fetch(a, ctx, c, token); err == nil {
				got.ConnectionID, got.Provider, got.Label, got.Source, got.FetchedAt = c.ID, c.Provider, c.Label, "api", q.FetchedAt
				if got.Windows == nil {
					got.Windows = []QuotaWindow{}
				}
				q = got
			}
		}
		if err != nil {
			q.Error = err.Error()
		}
	}
	if len(q.Windows) == 0 {
		if snap, ok := a.rate.get(c.ID); ok {
			q.Windows = windowsFromHeaders(snap.Headers)
			q.Source = "headers"
			if len(q.Windows) > 0 {
				q.Error = ""
			}
		}
	}
	a.quota.mu.Lock()
	a.quota.m[c.ID] = q
	a.quota.mu.Unlock()
	return q
}

// quotaList serves every active account's quota, read in parallel.
// Query: provider, refresh=1.
func (a *api) quotaList(w http.ResponseWriter, r *http.Request) {
	conns, err := a.store.ListConnections()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read connections")
		return
	}
	want := r.URL.Query().Get("provider")
	refresh := r.URL.Query().Get("refresh") == "1"
	var list []store.Connection
	for _, c := range conns {
		if _, ok := a.providerFor(c); ok && c.IsActive && (want == "" || c.Provider == want) {
			list = append(list, c)
		}
	}
	out := make([]AccountQuota, len(list))
	var wg sync.WaitGroup
	for i, c := range list {
		wg.Add(1)
		go func(i int, c store.Connection) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			out[i] = a.quotaFor(ctx, c, refresh)
		}(i, c)
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Label < out[j].Label
	})
	writeJSON(w, map[string]any{"accounts": out})
}

// windowsFromHeaders reads the rate-limit headers providers send with each
// answer: OpenAI-style x-ratelimit-{limit,remaining,reset}-{requests,tokens},
// Codex's x-codex-{primary,secondary}-*, Copilot's x-quota-snapshot-*.
// It never returns nil: the JSON of one account with null windows breaks the
// whole Quota page.
func windowsFromHeaders(h map[string]string) []QuotaWindow {
	out := []QuotaWindow{}
	// Codex: primary (short) and secondary (weekly) windows.
	for _, w := range []string{"primary", "secondary"} {
		used, ok := h["x-codex-"+w+"-used-percent"]
		if !ok {
			continue
		}
		pct, _ := strconv.ParseFloat(used, 64)
		qw := QuotaWindow{Name: w, UsedPct: pct}
		if mins, err := strconv.Atoi(h["x-codex-"+w+"-window-minutes"]); err == nil && mins > 0 {
			qw.Name = windowName(mins)
		}
		if at, err := strconv.ParseInt(h["x-codex-"+w+"-reset-at"], 10, 64); err == nil && at > 0 {
			qw.ResetAt = time.Unix(at, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, qw)
	}
	// OpenAI-style request and token limits.
	for _, kind := range []string{"requests", "tokens"} {
		limit, remaining := h["x-ratelimit-limit-"+kind], h["x-ratelimit-remaining-"+kind]
		if limit == "" || remaining == "" {
			continue
		}
		l, _ := strconv.ParseFloat(limit, 64)
		rm, _ := strconv.ParseFloat(remaining, 64)
		qw := QuotaWindow{Name: kind, UsedPct: -1, Limit: limit, Used: strconv.FormatFloat(l-rm, 'f', -1, 64)}
		if l > 0 {
			qw.UsedPct = 100 * (l - rm) / l
		}
		if d := h["x-ratelimit-reset-"+kind]; d != "" {
			if dur, err := time.ParseDuration(d); err == nil {
				qw.ResetAt = time.Now().Add(dur).UTC().Format(time.RFC3339)
			}
		}
		out = append(out, qw)
	}
	// Copilot: x-quota-snapshot-<name>: ent=…&rem=…&ov=…&rst=… style values.
	for k, v := range h {
		name, ok := strings.CutPrefix(k, "x-quota-snapshot-")
		if !ok {
			continue
		}
		vals := map[string]string{}
		for _, part := range strings.Split(v, "&") {
			if kk, vv, ok := strings.Cut(part, "="); ok {
				vals[kk] = vv
			}
		}
		qw := QuotaWindow{Name: name, UsedPct: -1}
		if vals["ent"] == "-1" {
			qw.Unlimited = true
		}
		// ent is the entitlement (the window's total), rem the remaining count,
		// so used percent is (ent-rem)/ent. rem is a count, not a percent: on a
		// 500-unit window with 450 left, 100-rem would read -350.
		if rem, err := strconv.ParseFloat(vals["rem"], 64); err == nil && !qw.Unlimited {
			qw.Limit = vals["ent"]
			if ent, err := strconv.ParseFloat(vals["ent"], 64); err == nil && ent > 0 {
				qw.UsedPct = 100 * (ent - rem) / ent
				qw.Used = strconv.FormatFloat(ent-rem, 'f', -1, 64)
			}
		}
		if rst := vals["rst"]; rst != "" {
			qw.ResetAt = rst
		}
		out = append(out, qw)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// windowName names a window by its length: 300 minutes is "5h", 10080 "7d".
func windowName(mins int) string {
	switch {
	case mins%1440 == 0:
		return strconv.Itoa(mins/1440) + "d"
	case mins%60 == 0:
		return strconv.Itoa(mins/60) + "h"
	}
	return strconv.Itoa(mins) + "m"
}
