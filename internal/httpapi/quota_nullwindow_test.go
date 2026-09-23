package httpapi

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// A snapshot that holds only headers no window can be read from must still
// serialize as an empty list. A null there breaks the whole Quota page.
func TestQuotaWindowsNeverNull(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	a, _ := newServer(s, nil, nil)
	c := store.Connection{ID: "c1", Provider: "groq", Label: "a"}
	a.rate.m[c.ID] = rateSnapshot{At: time.Now().UTC(), Headers: map[string]string{"retry-after": "5"}}

	q := a.quotaFor(context.Background(), c, true)
	if q.Windows == nil {
		t.Errorf("Windows = nil, want an empty slice")
	}
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"windows":[]`) {
		t.Errorf("json = %s, want \"windows\":[]", b)
	}
}

// windowsFromHeaders is the other seam: it must not hand a nil slice out.
func TestWindowsFromHeadersEmptyNotNil(t *testing.T) {
	if got := windowsFromHeaders(map[string]string{"retry-after": "5"}); got == nil {
		t.Errorf("windowsFromHeaders = nil, want an empty slice")
	}
}
