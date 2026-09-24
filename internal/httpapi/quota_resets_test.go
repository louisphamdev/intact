package httpapi

import (
	"context"
	"encoding/json"
	"github.com/louisphamdev/intact/internal/provider"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// Mapper test for Claude reset rows.
func TestClaudeResetRowsMapper(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	// 1. Null / missing blocks produce no rows.
	rows := claudeResetRows(map[string]any{}, now)
	if len(rows) != 0 {
		t.Errorf("empty usage: expected 0 rows, got %d", len(rows))
	}

	// 2. juniper_tide and cedar_ember mapping
	usage := map[string]any{
		"juniper_tide": map[string]any{
			"eligible":         true,
			"available":        true,
			"resets_per_week":  json.Number("1"),
			"weekly_resets_at": "2026-09-28T00:00:00Z",
		},
		"cedar_ember": map[string]any{
			"eligible": true,
			"grants": []any{
				map[string]any{
					"id":           "grant_1",
					"label":        "Standard Grant",
					"resets_total": json.Number("5"),
					"resets_left":  json.Number("3"),
					"starts_at":    "2026-09-20T00:00:00Z",
					"ends_at":      "2026-09-30T00:00:00Z",
					"clears":       []any{"five_hour", "seven_day"},
					"usable_now":   true,
				},
				map[string]any{
					"id":           "grant_paused",
					"label":        "Paused Grant",
					"resets_total": json.Number("2"),
					"resets_left":  json.Number("2"),
					"starts_at":    "2026-09-20T00:00:00Z",
					"ends_at":      "2026-09-30T00:00:00Z",
					"clears":       []any{"five_hour"},
					"usable_now":   false,
					"paused":       true,
				},
				map[string]any{
					"id":           "GRANT-UPPER",
					"label":        "Uppercase Grant",
					"resets_total": json.Number("1"),
					"resets_left":  json.Number("1"),
					"usable_now":   true,
				},
			},
		},
	}

	rows = claudeResetRows(usage, now)
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}

	// Weekly row
	w := rows[0]
	if w.ID != "weekly" || w.Kind != "weekly" || !w.Usable || w.Blocked != "" || w.Left != 1 || w.Total != 1 {
		t.Errorf("weekly row mismatch: %+v", w)
	}
	if len(w.Clears) != 1 || w.Clears[0] != "5h" {
		t.Errorf("weekly Clears = %+v, want [5h]", w.Clears)
	}

	// Standard grant
	g1 := rows[1]
	if g1.ID != "grant:grant_1" || g1.Kind != "grant" || !g1.Usable || g1.Blocked != "" || g1.Left != 3 || g1.Total != 5 {
		t.Errorf("g1 mismatch: %+v", g1)
	}
	if len(g1.Clears) != 2 || g1.Clears[0] != "5h" || g1.Clears[1] != "7d" {
		t.Errorf("g1 Clears = %+v, want [5h 7d]", g1.Clears)
	}

	// Paused grant
	gp := rows[2]
	if gp.ID != "grant:grant_paused" || gp.Usable || gp.Blocked != "paused" {
		t.Errorf("gp mismatch: %+v", gp)
	}

	// Uppercase grant id -> bad_id
	gu := rows[3]
	if gu.ID != "grant:GRANT-UPPER" || gu.Usable || gu.Blocked != "bad_id" {
		t.Errorf("gu mismatch: %+v", gu)
	}

	// 3. Ineligible blocks
	ineligUsage := map[string]any{
		"juniper_tide": map[string]any{
			"eligible":          false,
			"ineligible_reason": "surface",
		},
		"cedar_ember": map[string]any{
			"eligible":          false,
			"ineligible_reason": "not_subscribed",
			"grants": []any{
				map[string]any{
					"id":         "grant_x",
					"usable_now": true,
				},
			},
		},
	}
	ineligRows := claudeResetRows(ineligUsage, now)
	if len(ineligRows) != 2 {
		t.Fatalf("ineligible: expected 2 rows, got %d", len(ineligRows))
	}
	if ineligRows[0].Usable || ineligRows[0].Blocked != "ineligible:surface" {
		t.Errorf("ineligible weekly: %+v", ineligRows[0])
	}
	if ineligRows[1].Usable || ineligRows[1].Blocked != "ineligible:not_subscribed" {
		t.Errorf("ineligible grant: %+v", ineligRows[1])
	}
}

// Mapper test for Codex reset rows.
func TestCodexResetRowsMapper(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	clears := []string{"5h", "7d"}

	credits := map[string]any{
		"credits": []any{
			map[string]any{
				"id":          "credit-1",
				"status":      "available",
				"granted_at":  "2026-09-01T00:00:00Z",
				"expires_at":  "2026-10-01T00:00:00Z",
				"title":       "Credit 1",
				"description": "Desc 1",
			},
			map[string]any{
				"id":         "credit-redeeming",
				"status":     "redeeming",
				"expires_at": "2026-10-01T00:00:00Z",
			},
			map[string]any{
				"id":         "credit-redeemed",
				"status":     "redeemed",
				"expires_at": "2026-10-01T00:00:00Z",
			},
			map[string]any{
				"id":         "credit-expired",
				"status":     "available",
				"expires_at": "2026-09-01T00:00:00Z", // In the past relative to now
			},
			map[string]any{
				"id":         "credit-unknown-status",
				"status":     "something_weird",
				"expires_at": "2026-10-01T00:00:00Z",
			},
		},
	}

	rows := codexResetRows(credits, now, clears)
	if len(rows) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(rows))
	}

	// 1. available
	if r := rows[0]; r.ID != "credit:credit-1" || !r.Usable || r.Blocked != "" || r.Left != 1 || r.Total != 1 || r.Status != "available" {
		t.Errorf("credit 0 mismatch: %+v", r)
	}
	// 2. redeeming -> in_progress, Left == 0
	if r := rows[1]; r.Usable || r.Blocked != "in_progress" || r.Left != 0 || r.Title != "Full reset" || r.Description != "Reset your current usage limits" {
		t.Errorf("credit 1 mismatch: %+v", r)
	}
	// 3. redeemed -> used, Left == 0
	if r := rows[2]; r.Usable || r.Blocked != "used" || r.Left != 0 {
		t.Errorf("credit 2 mismatch: %+v", r)
	}
	// 4. expired -> expired
	if r := rows[3]; r.Usable || r.Blocked != "expired" {
		t.Errorf("credit 3 mismatch: %+v", r)
	}
	// 5. unknown_status, Left == 0
	if r := rows[4]; r.Usable || r.Blocked != "unknown_status" || r.Left != 0 {
		t.Errorf("credit 4 mismatch: %+v", r)
	}
}

// Live-like fake test: Claude read carries Identity User-Agent and query parameters.
func TestQuotaClaudeReadPathIdentityAndResets(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var gotUA, gotQuery atomic.Pointer[string]
	fakeClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		gotUA.Store(&ua)
		q := r.URL.RawQuery
		gotQuery.Store(&q)

		resp := map[string]any{
			"five_hour": map[string]any{
				"utilization": json.Number("25.5"),
				"resets_at":   "2026-09-23T15:00:00Z",
			},
			"juniper_tide": map[string]any{
				"eligible":        true,
				"available":       true,
				"resets_per_week": json.Number("1"),
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer fakeClaude.Close()

	a, _ := newServer(s, map[string]string{"claude": fakeClaude.URL}, nil)
	c, _ := s.CreateConnection("claude", "Personal", "token-xyz")

	q := a.quotaFor(context.Background(), c, true)
	if q.Error != "" {
		t.Fatalf("quotaFor error: %s", q.Error)
	}

	if gotQuery.Load() == nil || !strings.Contains(*gotQuery.Load(), "at_wall=1") || !strings.Contains(*gotQuery.Load(), "skip_spend=1") {
		t.Errorf("query = %v, want at_wall=1&skip_spend=1", gotQuery.Load())
	}
	// Anthropic offers resets only to the interactive CLI surface; "sdk-cli" gets ineligible:surface.
	if gotUA.Load() == nil || *gotUA.Load() != provider.ClaudeInteractiveUserAgent || !strings.HasSuffix(*gotUA.Load(), "(external, cli)") {
		t.Errorf("User-Agent = %v, want the interactive CLI UA", gotUA.Load())
	}
	if len(q.Windows) != 1 || q.Windows[0].Name != "5h" {
		t.Errorf("Windows mismatch: %+v", q.Windows)
	}
	if len(q.Resets) != 1 || q.Resets[0].ID != "weekly" {
		t.Errorf("Resets mismatch: %+v", q.Resets)
	}
}

// Live-like fake test: Codex read path, available_count: 0 vs >0, and failed credits call.
func TestQuotaCodexReadPathCredits(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var creditsCalls atomic.Int32
	var creditsFail atomic.Bool
	var availableCount atomic.Int32

	fakeCodex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wham/usage") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "plus",
				"rate_limit": map[string]any{
					"primary_window": map[string]any{
						"used_percent":         json.Number("40"),
						"limit_window_seconds": json.Number("18000"), // 300 mins = 5h
					},
					"secondary_window": map[string]any{
						"used_percent":         json.Number("60"),
						"limit_window_seconds": json.Number("604800"), // 10080 mins = 7d
					},
				},
				"rate_limit_reset_credits": map[string]any{
					"available_count": json.Number(string(rune('0' + availableCount.Load()))),
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/wham/rate-limit-reset-credits") {
			creditsCalls.Add(1)
			if creditsFail.Load() {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"credits": []any{
					map[string]any{
						"id":         "credit-abc",
						"status":     "available",
						"expires_at": "2026-10-23T00:00:00Z",
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeCodex.Close()

	a, _ := newServer(s, map[string]string{"codex": fakeCodex.URL}, nil)
	c, _ := s.CreateConnection("codex", "Work", "token-123")

	// Case 1: available_count = 0 -> no second call
	availableCount.Store(0)
	q1 := a.quotaFor(context.Background(), c, true)
	if q1.Error != "" {
		t.Fatalf("quotaFor error: %s", q1.Error)
	}
	if creditsCalls.Load() != 0 {
		t.Errorf("creditsCalls = %d, want 0 when available_count is 0", creditsCalls.Load())
	}
	if len(q1.Resets) != 0 {
		t.Errorf("expected 0 resets, got %+v", q1.Resets)
	}

	// Case 2: available_count = 1 -> second call made and mapped
	availableCount.Store(1)
	q2 := a.quotaFor(context.Background(), c, true)
	if q2.Error != "" {
		t.Fatalf("quotaFor error: %s", q2.Error)
	}
	if creditsCalls.Load() != 1 {
		t.Errorf("creditsCalls = %d, want 1", creditsCalls.Load())
	}
	if len(q2.Resets) != 1 || q2.Resets[0].ID != "credit:credit-abc" {
		t.Errorf("expected credit-abc reset, got %+v", q2.Resets)
	}
	if len(q2.Resets[0].Clears) != 2 || q2.Resets[0].Clears[0] != "5h" || q2.Resets[0].Clears[1] != "7d" {
		t.Errorf("Clears = %+v, want [5h 7d]", q2.Resets[0].Clears)
	}

	// Case 3: credits call fails -> windows kept, ResetsError set
	creditsFail.Store(true)
	q3 := a.quotaFor(context.Background(), c, true)
	if q3.Error != "" {
		t.Errorf("q3.Error = %q, want empty (windows should still succeed)", q3.Error)
	}
	if len(q3.Windows) != 2 {
		t.Errorf("len(q3.Windows) = %d, want 2", len(q3.Windows))
	}
	if q3.ResetsError == "" {
		t.Error("q3.ResetsError empty, want error from failed credits call")
	}
}
