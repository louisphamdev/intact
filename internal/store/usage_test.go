package store

import (
	"path/filepath"
	"testing"
)

func TestAddUsageAggregatesPerDayAccountModel(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	day := "2026-09-21"
	// Two calls on the same account, same model, same day: they must fold into
	// one row that sums the tokens and counts two requests.
	if err := s.AddUsage(day, "acc-1", "claude-opus-4-8", 100, 40); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	if err := s.AddUsage(day, "acc-1", "claude-opus-4-8", 20, 5); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	// A different model on the same account and day is a separate row.
	if err := s.AddUsage(day, "acc-1", "llama-3.3-70b", 7, 3); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}

	rows, err := s.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Usage returned %d rows, want 2: %+v", len(rows), rows)
	}

	byModel := map[string]UsageRow{}
	for _, r := range rows {
		byModel[r.Model] = r
	}

	opus := byModel["claude-opus-4-8"]
	if opus.InputTokens != 120 || opus.OutputTokens != 45 || opus.Requests != 2 {
		t.Errorf("opus row = %+v, want input=120 output=45 requests=2", opus)
	}
	if opus.Day != day || opus.ConnectionID != "acc-1" {
		t.Errorf("opus row key = %q/%q, want %q/acc-1", opus.Day, opus.ConnectionID, day)
	}

	llama := byModel["llama-3.3-70b"]
	if llama.InputTokens != 7 || llama.OutputTokens != 3 || llama.Requests != 1 {
		t.Errorf("llama row = %+v, want input=7 output=3 requests=1", llama)
	}
}
