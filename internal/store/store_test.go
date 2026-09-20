package store

import (
	"path/filepath"
	"testing"
)

func TestOpenAppliesSchemaAndWAL(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var mode string
	if err := s.DB.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	var name string
	err = s.DB.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='connections'").Scan(&name)
	if err != nil {
		t.Fatalf("connections table missing: %v", err)
	}
}
