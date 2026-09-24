package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

// QuotaReset is one reset that an account can spend.
type QuotaReset struct {
	ID          string   `json:"id"`   // "weekly", "grant:<id>", "credit:<id>"
	Kind        string   `json:"kind"` // weekly | grant | credit
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Clears      []string `json:"clears"`
	Left        int      `json:"left"`
	Total       int      `json:"total"`
	ValidFrom   string   `json:"validFrom,omitempty"`
	ExpiresAt   string   `json:"expiresAt,omitempty"`
	NextAt      string   `json:"nextAt,omitempty"`
	Usable      bool     `json:"usable"`
	Blocked     string   `json:"blocked,omitempty"`
	Status      string   `json:"status,omitempty"`
}

var (
	claudeGrantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,128}$`)
	codexCreditIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

func cutString(s string, limit int) string {
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

func mapClaudeClearName(name string) string {
	switch {
	case name == "five_hour":
		return "5h"
	case name == "seven_day":
		return "7d"
	case strings.HasPrefix(name, "seven_day_"):
		return "7d " + strings.TrimPrefix(name, "seven_day_")
	}
	return name
}

// claudeResetRows maps juniper_tide and cedar_ember blocks to QuotaReset rows.
func claudeResetRows(usage map[string]any, now time.Time) []QuotaReset {
	var rows []QuotaReset

	// 1. juniper_tide (weekly reset)
	if jt := jobj(usage["juniper_tide"]); jt != nil {
		eligible := true
		if e, ok := jt["eligible"].(bool); ok && !e {
			eligible = false
		}
		ineligReason, _ := jt["ineligible_reason"].(string)

		available, _ := jt["available"].(bool)
		resetsPerWeek, _ := jnum(jt["resets_per_week"])
		nextAt, _ := jt["next_available_at"].(string)

		total := int(resetsPerWeek)
		if total < 0 {
			total = 0
		}
		left := 0
		if available {
			left = 1
		}

		w := QuotaReset{
			ID:     "weekly",
			Kind:   "weekly",
			Title:  "5-hour limit reset",
			Clears: []string{"5h"},
			Left:   left,
			Total:  total,
			NextAt: nextAt,
		}

		switch {
		case !eligible:
			w.Usable = false
			w.Blocked = "ineligible:" + ineligReason
		case !available && nextAt != "":
			w.Usable = false
			w.Blocked = "used"
		case !available:
			w.Usable = false
			w.Blocked = "not_at_limit"
		default:
			w.Usable = true
			w.Blocked = ""
		}
		rows = append(rows, w)
	}

	// 2. cedar_ember (grants)
	if ce := jobj(usage["cedar_ember"]); ce != nil {
		emberEligible := true
		if e, ok := ce["eligible"].(bool); ok && !e {
			emberEligible = false
		}
		emberReason, _ := ce["ineligible_reason"].(string)

		isCooldown := false
		if cu, ok := ce["cooldown_until"].(string); ok && cu != "" {
			if t, err := time.Parse(time.RFC3339, cu); err == nil && now.Before(t) {
				isCooldown = true
			}
		}

		rawGrants, _ := ce["grants"].([]any)
		seenIDs := map[string]bool{}

		for i, item := range rawGrants {
			gm := jobj(item)
			if gm == nil {
				continue
			}
			pos := i + 1
			id, _ := gm["id"].(string)
			label, _ := gm["label"].(string)
			if label == "" {
				label = "Grant"
			}
			label = cutString(label, 200)

			leftVal, _ := jnum(gm["resets_left"])
			totalVal, _ := jnum(gm["resets_total"])
			left := int(leftVal)
			total := int(totalVal)
			if left < 0 {
				left = 0
			}
			if total < 0 {
				total = 0
			}

			startsAt, _ := gm["starts_at"].(string)
			endsAt, _ := gm["ends_at"].(string)
			paused, _ := gm["paused"].(bool)
			usableNow, _ := gm["usable_now"].(bool)

			var clears []string
			if rawClears, ok := gm["clears"].([]any); ok {
				for _, c := range rawClears {
					if cs, ok := c.(string); ok {
						clears = append(clears, cutString(mapClaudeClearName(cs), 40))
					}
				}
			}
			if clears == nil {
				clears = []string{}
			}

			rowID := "grant:" + id
			badID := false
			if id == "" || !claudeGrantIDPattern.MatchString(id) || seenIDs[id] {
				badID = true
				if id == "" || seenIDs[id] {
					rowID = fmt.Sprintf("bad:%d", pos)
				}
			}
			if id != "" {
				seenIDs[id] = true
			}

			r := QuotaReset{
				ID:        rowID,
				Kind:      "grant",
				Title:     label,
				Clears:    clears,
				Left:      left,
				Total:     total,
				ValidFrom: startsAt,
				ExpiresAt: endsAt,
			}

			if badID {
				r.Usable = false
				r.Blocked = "bad_id"
			} else {
				// Claude blocked rule order
				var notStarted, isExpired bool
				if startsAt != "" {
					if t, err := time.Parse(time.RFC3339, startsAt); err == nil && now.Before(t) {
						notStarted = true
					}
				}
				if endsAt != "" {
					if t, err := time.Parse(time.RFC3339, endsAt); err == nil && !now.Before(t) {
						isExpired = true
					}
				}

				switch {
				case !emberEligible:
					r.Usable = false
					r.Blocked = "ineligible:" + emberReason
				case isCooldown:
					r.Usable = false
					r.Blocked = "cooldown"
				case paused:
					r.Usable = false
					r.Blocked = "paused"
				case notStarted:
					r.Usable = false
					r.Blocked = "not_started"
				case isExpired:
					r.Usable = false
					r.Blocked = "expired"
				case left == 0:
					r.Usable = false
					r.Blocked = "used"
				case !usableNow:
					r.Usable = false
					r.Blocked = "not_at_limit"
				default:
					r.Usable = true
					r.Blocked = ""
				}
			}
			rows = append(rows, r)
		}
	}

	return rows
}

// codexResetRows maps the Codex rate-limit-reset-credits response to QuotaReset rows.
func codexResetRows(credits map[string]any, now time.Time, clears ...[]string) []QuotaReset {
	var defaultClears []string
	if len(clears) > 0 && clears[0] != nil {
		for _, c := range clears[0] {
			defaultClears = append(defaultClears, cutString(c, 40))
		}
	}
	if defaultClears == nil {
		defaultClears = []string{"5h", "7d"}
	}

	rawList, _ := credits["credits"].([]any)
	var rows []QuotaReset
	seenIDs := map[string]bool{}

	for i, item := range rawList {
		cm := jobj(item)
		if cm == nil {
			continue
		}
		pos := i + 1
		id, _ := cm["id"].(string)
		status, _ := cm["status"].(string)
		title, _ := cm["title"].(string)
		if title == "" {
			title = "Full reset"
		}
		title = cutString(title, 200)

		desc, _ := cm["description"].(string)
		if desc == "" {
			desc = "Reset your current usage limits"
		}
		desc = cutString(desc, 1000)

		grantedAt, _ := cm["granted_at"].(string)
		expiresAt, _ := cm["expires_at"].(string)

		rowID := "credit:" + id
		badID := false
		if id == "" || !codexCreditIDPattern.MatchString(id) || seenIDs[id] {
			badID = true
			if id == "" || seenIDs[id] {
				rowID = fmt.Sprintf("bad:%d", pos)
			}
		}
		if id != "" {
			seenIDs[id] = true
		}

		r := QuotaReset{
			ID:          rowID,
			Kind:        "credit",
			Title:       title,
			Description: desc,
			Clears:      defaultClears,
			Left:        1,
			Total:       1,
			Status:      status,
			ValidFrom:   grantedAt,
			ExpiresAt:   expiresAt,
		}
		if status != "available" {
			r.Left = 0
		}

		if badID {
			r.Usable = false
			r.Blocked = "bad_id"
		} else {
			isExpired := false
			if expiresAt != "" {
				if t, err := time.Parse(time.RFC3339, expiresAt); err == nil && !now.Before(t) {
					isExpired = true
				}
			}

			if isExpired {
				r.Usable = false
				r.Blocked = "expired"
			} else {
				switch status {
				case "available":
					r.Usable = true
					r.Blocked = ""
				case "redeeming":
					r.Usable = false
					r.Blocked = "in_progress"
				case "redeemed":
					r.Usable = false
					r.Blocked = "used"
				default:
					r.Usable = false
					r.Blocked = "unknown_status"
				}
			}
		}

		rows = append(rows, r)
	}

	return rows
}

var (
	uuidRegex        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
)

func isUUID(s string) bool {
	return uuidRegex.MatchString(s)
}

// claudeOrgID returns the Claude organization UUID for a connection, reading
// meta.claudeOrgId first, or fetching it from the profile endpoint.
func (a *api) claudeOrgID(ctx context.Context, c store.Connection, token string) (string, error) {
	conns, _ := a.store.ListConnections()
	for _, conn := range conns {
		if conn.ID == c.ID {
			if orgID, ok := conn.Meta["claudeOrgId"]; ok && orgID != "" && isUUID(orgID) {
				return orgID, nil
			}
			break
		}
	}

	u := claudeProfileURL
	if over, ok := a.baseOverride["claude"]; ok {
		u = over + "/api/oauth/profile"
	}
	h := map[string]string{
		"Authorization":     "Bearer " + token,
		"Anthropic-Beta":    "oauth-2025-04-20",
		"Anthropic-Version": "2023-06-01",
	}
	if p, ok := provider.Lookup("claude"); ok {
		for k, v := range p.Identity {
			h[k] = v
		}
	}
	h["User-Agent"] = provider.ClaudeInteractiveUserAgent

	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	var prof struct {
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if _, err := fetchJSON(pctx, "GET", u, h, nil, &prof); err != nil {
		return "", err
	}

	uuid := prof.Organization.UUID
	if !isUUID(uuid) {
		return "", fmt.Errorf("profile organization.uuid %q is not a valid UUID", uuid)
	}

	if err := a.store.SetMeta(c.ID, map[string]string{"claudeOrgId": uuid}); err != nil {
		log.Printf("store claudeOrgId for %s: %v", c.ID, err)
	}
	return uuid, nil
}

// claimResult is the normalized outcome from a provider claim attempt.
type claimResult struct {
	Outcome      string // reset, not_needed, spent, not_allowed, failed, unknown, likely_spent
	ProviderCode string
	Message      string
}

// claimTables holds the unknown, hold, and done claim tables guarded by a single mutex.
type claimTables struct {
	mu      sync.Mutex
	unknown map[string]claimUnknownEntry // key: connID + ":" + resetId
	hold    map[string]claimHoldEntry    // key: connID + ":" + resetId
	done    map[string]claimDoneEntry    // key: connID + ":" + requestId
}

type claimUnknownEntry struct {
	requestID string
	at        time.Time
	left      int
}

type claimHoldEntry struct {
	left int
	at   time.Time
}

type claimDoneEntry struct {
	outcome string
	at      time.Time
}

var (
	claimRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	resetClaimers         = map[string]func(ctx context.Context, a *api, c store.Connection, token string, r QuotaReset, requestID string) (claimResult, error){}
)

func init() {
	resetClaimers["claude"] = claimClaude
	resetClaimers["codex"] = claimCodex
}

// claimClaude performs one claim against Anthropic's reset_rate_limits endpoint.
func claimClaude(ctx context.Context, a *api, c store.Connection, token string, r QuotaReset, requestID string) (claimResult, error) {
	orgID, err := a.claudeOrgID(ctx, c, token)
	if err != nil {
		pCode := ""
		if strings.HasPrefix(err.Error(), "401:") {
			pCode = "401"
		}
		return claimResult{Outcome: "failed", ProviderCode: pCode, Message: err.Error()}, nil
	}

	u := "https://api.anthropic.com/api/organizations/" + orgID + "/reset_rate_limits"
	if over, ok := a.baseOverride["claude"]; ok {
		u = over + "/api/organizations/" + orgID + "/reset_rate_limits"
	}

	h := map[string]string{
		"Authorization":     "Bearer " + token,
		"Anthropic-Beta":    "oauth-2025-04-20",
		"Anthropic-Version": "2023-06-01",
	}
	if p, ok := provider.Lookup("claude"); ok {
		for k, v := range p.Identity {
			h[k] = v
		}
	}
	h["User-Agent"] = provider.ClaudeInteractiveUserAgent

	var reqBody any
	if r.Kind == "weekly" {
		reqBody = map[string]any{"program": "juniper_tide"}
	} else if r.Kind == "grant" {
		grantID := strings.TrimPrefix(r.ID, "grant:")
		reqBody = map[string]any{
			"program":    "cedar_ember",
			"grant_id":   grantID,
			"request_id": requestID,
		}
	} else {
		return claimResult{Outcome: "failed", Message: "unknown reset kind"}, nil
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
	defer cancel()

	var d map[string]any
	_, ferr := fetchJSON(cctx, "POST", u, h, reqBody, &d)
	if ferr != nil {
		msg := ferr.Error()
		if strings.HasPrefix(msg, "401:") {
			return claimResult{Outcome: "failed", ProviderCode: "401", Message: msg}, nil
		}
		if strings.HasPrefix(msg, "403:") {
			_ = a.store.SetMeta(c.ID, map[string]string{"claudeOrgId": ""})
			return claimResult{Outcome: "failed", ProviderCode: "403", Message: msg}, nil
		}
		if strings.HasPrefix(msg, "404:") {
			_ = a.store.SetMeta(c.ID, map[string]string{"claudeOrgId": ""})
			return claimResult{Outcome: "unknown", ProviderCode: "404", Message: msg}, nil
		}
		if strings.HasPrefix(msg, "429:") {
			return claimResult{Outcome: "failed", ProviderCode: "429", Message: msg}, nil
		}
		return claimResult{Outcome: "unknown", Message: msg}, nil
	}

	resStr, _ := d["result"].(string)
	reasonStr, _ := d["reason"].(string)

	switch resStr {
	case "reset":
		return claimResult{Outcome: "reset", ProviderCode: resStr, Message: reasonStr}, nil
	case "not_limited":
		return claimResult{Outcome: "not_needed", ProviderCode: resStr, Message: reasonStr}, nil
	case "already_used":
		return claimResult{Outcome: "spent", ProviderCode: resStr, Message: reasonStr}, nil
	case "ineligible", "cooldown":
		return claimResult{Outcome: "not_allowed", ProviderCode: resStr, Message: reasonStr}, nil
	default:
		return claimResult{Outcome: "unknown", ProviderCode: resStr, Message: reasonStr}, nil
	}
}

// claimCodex performs one claim against Codex rate-limit-reset-credits consume endpoint.
func claimCodex(ctx context.Context, a *api, c store.Connection, token string, r QuotaReset, requestID string) (claimResult, error) {
	u := "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	if over, ok := a.baseOverride["codex"]; ok {
		u = over + "/backend-api/wham/rate-limit-reset-credits/consume"
	}

	h := map[string]string{"Authorization": "Bearer " + token}
	if id := chatgptAccountID(token); id != "" {
		h["ChatGPT-Account-ID"] = id
	}

	creditID := strings.TrimPrefix(r.ID, "credit:")
	reqBody := map[string]any{
		"redeem_request_id": requestID,
		"credit_id":         creditID,
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
	defer cancel()

	var d map[string]any
	_, ferr := fetchJSON(cctx, "POST", u, h, reqBody, &d)
	if ferr != nil {
		msg := ferr.Error()
		if strings.HasPrefix(msg, "401:") {
			return claimResult{Outcome: "failed", ProviderCode: "401", Message: msg}, nil
		}
		if strings.HasPrefix(msg, "403:") {
			return claimResult{Outcome: "failed", ProviderCode: "403", Message: msg}, nil
		}
		if strings.HasPrefix(msg, "429:") {
			return claimResult{Outcome: "failed", ProviderCode: "429", Message: msg}, nil
		}
		return claimResult{Outcome: "unknown", Message: msg}, nil
	}

	code, _ := d["code"].(string)
	switch code {
	case "reset":
		return claimResult{Outcome: "reset", ProviderCode: code}, nil
	case "nothing_to_reset":
		return claimResult{Outcome: "not_needed", ProviderCode: code}, nil
	case "already_redeemed", "no_credit":
		return claimResult{Outcome: "spent", ProviderCode: code}, nil
	default:
		return claimResult{Outcome: "unknown", ProviderCode: code}, nil
	}
}

// claimReset handles POST /quota/{connectionId}/reset.
func (a *api) claimReset(w http.ResponseWriter, r *http.Request) {
	// 1. Auth: No-auth mode or no valid session cookie answers 403.
	if a.auth == nil {
		writeError(w, http.StatusForbidden, "claims need sign-in")
		return
	}
	ck, err := r.Cookie(sessionCookie)
	if err != nil || !a.auth.ValidSession(ck.Value) {
		writeError(w, http.StatusForbidden, "claims need sign-in")
		return
	}

	// Sec-Fetch-Site check: if present and not same-origin, 403.
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
		writeError(w, http.StatusForbidden, "cross-site request refused")
		return
	}

	// Content-Type check.
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusBadRequest, "Content-Type must be application/json")
		return
	}

	// Path parameter connection id.
	connID := r.PathValue("id")
	if connID == "" {
		connID = r.PathValue("connectionId")
	}
	if connID == "" {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}

	conn, err := a.connection(connID)
	if err != nil || !conn.IsActive {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}

	claimer, hasClaimer := resetClaimers[conn.Provider]
	if !hasClaimer {
		writeError(w, http.StatusNotFound, "provider has no claimer")
		return
	}

	// Body max 4 KiB.
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		ResetID   string `json:"resetId"`
		RequestID string `json:"requestId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json body: "+err.Error())
		return
	}
	if len(body.ResetID) == 0 || len(body.ResetID) > 200 {
		writeError(w, http.StatusBadRequest, "resetId must be 1-200 characters")
		return
	}
	if !claimRequestIDPattern.MatchString(body.RequestID) {
		writeError(w, http.StatusBadRequest, "invalid requestId format")
		return
	}

	// 2. Lock: tryLock per connection.
	l, ok := a.claimLocks.tryLock(connID)
	if !ok {
		writeConflictError(w, map[string]string{"error": "claim_in_progress"})
		return
	}
	defer l.Unlock()

	resetKey := connID + ":" + body.ResetID
	reqKey := connID + ":" + body.RequestID
	now := time.Now()

	// 3. Unknown and done check.
	a.claims.mu.Lock()
	// Cleanup expired done entries (> 15 minutes)
	for k, de := range a.claims.done {
		if now.Sub(de.at) > 15*time.Minute {
			delete(a.claims.done, k)
		}
	}
	// Check done table
	if de, ok := a.claims.done[reqKey]; ok {
		a.claims.mu.Unlock()
		writeConflictError(w, map[string]string{
			"error":   "already_done",
			"outcome": de.outcome,
		})
		return
	}

	// Check unknown table
	ue, hasUnknown := a.claims.unknown[resetKey]
	if hasUnknown {
		if ue.requestID != body.RequestID {
			a.claims.mu.Unlock()
			writeConflictError(w, map[string]string{
				"error":     "unknown_pending",
				"requestId": ue.requestID,
			})
			return
		}
		if now.Sub(ue.at) < 60*time.Second {
			a.claims.mu.Unlock()
			writeConflictError(w, map[string]string{
				"error":     "unknown_pending",
				"requestId": ue.requestID,
			})
			return
		}
	}
	a.claims.mu.Unlock()

	// 4. Fresh read
	fresh, ferr := a.freshQuota(r.Context(), conn)
	if ferr != nil || fresh.ResetsError != "" {
		writeError(w, http.StatusServiceUnavailable, "cannot read resets now")
		return
	}
	if conn.Provider == "claude" {
		if body.ResetID == "weekly" && !fresh.HasJuniperTide {
			writeError(w, http.StatusServiceUnavailable, "cannot read resets now")
			return
		}
		if strings.HasPrefix(body.ResetID, "grant:") && !fresh.HasCedarEmber {
			writeError(w, http.StatusServiceUnavailable, "cannot read resets now")
			return
		}
	}

	// Find the targeted row in fresh.Resets
	var targetRow *QuotaReset
	for i := range fresh.Resets {
		if fresh.Resets[i].ID == body.ResetID {
			targetRow = &fresh.Resets[i]
			break
		}
	}

	// Check if this was an unknown retry after >= 60s
	if hasUnknown && ue.requestID == body.RequestID {
		isLikelySpent := false
		if targetRow == nil {
			isLikelySpent = true
		} else if targetRow.Kind == "weekly" {
			if targetRow.Left < ue.left && targetRow.NextAt != "" {
				isLikelySpent = true
			}
		} else {
			if targetRow.Left < ue.left {
				isLikelySpent = true
			}
		}

		if isLikelySpent {
			a.claims.mu.Lock()
			delete(a.claims.unknown, resetKey)
			a.claims.done[reqKey] = claimDoneEntry{outcome: "likely_spent", at: time.Now()}
			a.claims.mu.Unlock()

			writeJSON(w, map[string]any{
				"outcome":   "likely_spent",
				"requestId": body.RequestID,
			})
			return
		}
	}

	// Check hold table against fresh read's Left
	a.claims.mu.Lock()
	if he, ok := a.claims.hold[resetKey]; ok {
		if targetRow != nil && targetRow.Left < he.left {
			delete(a.claims.hold, resetKey)
		} else if time.Since(he.at) < 60*time.Second {
			a.claims.mu.Unlock()
			writeConflictError(w, map[string]string{
				"error":   "just_reset",
				"blocked": "just_reset",
			})
			return
		} else {
			delete(a.claims.hold, resetKey)
		}
	}
	a.claims.mu.Unlock()

	if targetRow == nil {
		writeConflictError(w, map[string]string{
			"error":   "not_found",
			"blocked": "not_found",
		})
		return
	}

	if !targetRow.Usable {
		writeConflictError(w, map[string]string{
			"error":   targetRow.Blocked,
			"blocked": targetRow.Blocked,
		})
		return
	}

	// 5. Log
	log.Printf("claim sent: connection=%s resetId=%s requestId=%s", connID, body.ResetID, body.RequestID)

	// 6. Claim
	tctx, tcancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer tcancel()
	token, terr := a.secretFor(tctx, connID)
	if terr != nil {
		writeError(w, http.StatusServiceUnavailable, "cannot read connection token")
		return
	}

	claimRes, _ := claimer(r.Context(), a, conn, token, *targetRow, body.RequestID)
	if claimRes.Outcome == "failed" && claimRes.ProviderCode == "401" {
		go func() {
			rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rcancel()
			a.forceRefresh(rctx, connID)
		}()
	}

	// 7. Normalize & Log
	log.Printf("claim outcome: connection=%s resetId=%s requestId=%s outcome=%s providerCode=%s",
		connID, body.ResetID, body.RequestID, claimRes.Outcome, claimRes.ProviderCode)

	// 8. Record
	a.invalidateQuota(connID)

	a.claims.mu.Lock()
	switch claimRes.Outcome {
	case "reset":
		delete(a.claims.unknown, resetKey)
		a.claims.done[reqKey] = claimDoneEntry{outcome: "reset", at: time.Now()}
		a.claims.hold[resetKey] = claimHoldEntry{left: targetRow.Left, at: time.Now()}
	case "spent", "not_needed", "not_allowed":
		delete(a.claims.unknown, resetKey)
		a.claims.done[reqKey] = claimDoneEntry{outcome: claimRes.Outcome, at: time.Now()}
	case "unknown":
		a.claims.unknown[resetKey] = claimUnknownEntry{
			requestID: body.RequestID,
			at:        time.Now(),
			left:      targetRow.Left,
		}
	case "failed":
		// failed never enters done table; keeps existing unknown entry if any
	}
	a.claims.mu.Unlock()

	// 9. Refresh on reset
	resBody := map[string]any{
		"outcome":      claimRes.Outcome,
		"providerCode": claimRes.ProviderCode,
		"message":      claimRes.Message,
		"requestId":    body.RequestID,
	}

	if claimRes.Outcome == "reset" {
		if postFresh, err := a.freshQuota(r.Context(), conn); err == nil {
			resBody["quota"] = postFresh
		}
	}

	writeJSON(w, resBody)
}

func writeConflictError(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	b, _ := json.Marshal(body)
	w.Write(b)
}
