package store

import (
	"database/sql"
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
	if err := s.AddUsage(day, "acc-1", "claude-opus-4-8", 100, 40, 0); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	if err := s.AddUsage(day, "acc-1", "claude-opus-4-8", 20, 5, 0); err != nil {
		t.Fatalf("AddUsage: %v", err)
	}
	// A different model on the same account and day is a separate row.
	if err := s.AddUsage(day, "acc-1", "llama-3.3-70b", 7, 3, 0); err != nil {
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

// Cached tokens add up beside input and output, for an account and for a key.
func TestUsageCountsCachedTokens(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddUsage("2026-10-02", "acc-1", "m", 1000, 5, 896)
	s.AddUsage("2026-10-02", "acc-1", "m", 1000, 5, 0)
	rows, _ := s.Usage()
	if len(rows) != 1 || rows[0].InputTokens != 2000 || rows[0].CachedTokens != 896 || rows[0].Requests != 2 {
		t.Errorf("usage rows = %+v, want input 2000, cached 896, 2 requests", rows)
	}
	s.AddKeyUsage("2026-10-02", "k1", "m", 1000, 5, 768)
	krows, _ := s.KeyUsage("k1", "2026-10-01")
	if len(krows) != 1 || krows[0].CachedTokens != 768 {
		t.Errorf("key usage rows = %+v, want cached 768", krows)
	}
}

// A database from before the cached column opens, keeps its rows, and counts
// cached tokens from then on.
func TestUsageMigrationAddsCachedColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE usage_daily (day TEXT NOT NULL, connection_id TEXT NOT NULL, model TEXT NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			requests INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (day, connection_id, model))`,
		`INSERT INTO usage_daily VALUES ('2026-10-01', 'acc-1', 'm', 50, 5, 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	defer s.Close()
	if err := s.AddUsage("2026-10-02", "acc-1", "m", 100, 5, 64); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Usage()
	if len(rows) != 2 || rows[0].CachedTokens != 64 || rows[1].InputTokens != 50 || rows[1].CachedTokens != 0 {
		t.Errorf("rows after migration = %+v", rows)
	}
}
