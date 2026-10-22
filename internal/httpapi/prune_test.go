package httpapi

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// A restart resets the every-200-inserts counter, so a gateway that restarts
// more often than it logs 200 errors must still prune when it starts.
func TestStartPrunesOldUpstreamErrors(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	old := time.Now().UTC().AddDate(0, 0, -(errKeepDays + 1)).Format(time.RFC3339)
	s.AddUpstreamError(store.UpstreamError{Provider: "groq", Model: "m", Status: 500, At: old})
	s.AddUpstreamError(store.UpstreamError{Provider: "groq", Model: "m", Status: 500})

	New(s, nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		s.DB.QueryRow(`SELECT count(*) FROM upstream_errors`).Scan(&n)
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream_errors = %d rows after start, want 1 (the old row pruned)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
