package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestUsageEndpointReturnsDailyRows(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	if err := s.AddUsage("2026-09-21", "acc-1", "claude-opus-4-8", 120, 45); err != nil {
		t.Fatal(err)
	}

	h := New(s, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/usage", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got struct {
		Usage []store.UsageRow `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Usage) != 1 || got.Usage[0].Model != "claude-opus-4-8" ||
		got.Usage[0].InputTokens != 120 || got.Usage[0].OutputTokens != 45 {
		t.Errorf("usage = %+v", got.Usage)
	}
}
