package httpapi

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// Bug G: a store write failure must be logged, not swallowed, so a lock or a
// full disk is visible instead of usage vanishing in silence.
func TestRecordUsageLogsStoreError(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	c, _ := s.CreateConnection("groq", "test", "gsk-abc")
	s.Close() // any write now fails

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	a := &api{store: s}
	a.recordUsage(c.ID, []byte(`{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":1}}`), "")

	if !strings.Contains(logBuf.String(), "record usage") {
		t.Errorf("store error was not logged: %q", logBuf.String())
	}
}
