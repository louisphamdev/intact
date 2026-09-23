package store

import (
	"os"
	"path/filepath"
	"syscall"
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

func TestOpenDatabasePermissionsUnderUmask022(t *testing.T) {
	oldUmask := syscall.Umask(0o022)
	defer syscall.Umask(oldUmask)

	// New temp path, Open + one write -> db/-wal/-shm perm&0o077==0
	dbPath := filepath.Join(t.TempDir(), "new.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if _, err := s.DB.Exec("INSERT INTO connections (id, provider, label, secret, created_at, updated_at) VALUES ('c1', 'groq', 'l1', 'k1', 'now', 'now')"); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) && p != dbPath {
				continue
			}
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("file %s has perm %04o; want perm&0o077==0", p, perm)
		}
	}

	// Pre-created 0644 db -> 0600 after Open
	prePath := filepath.Join(t.TempDir(), "precreated.db")
	if err := os.WriteFile(prePath, []byte{}, 0o644); err != nil {
		t.Fatalf("create prePath: %v", err)
	}
	sPre, err := Open(prePath)
	if err != nil {
		t.Fatalf("Open prePath: %v", err)
	}
	defer sPre.Close()

	info, err := os.Stat(prePath)
	if err != nil {
		t.Fatalf("stat prePath: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("precreated db has perm %04o; want 0600", perm)
	}

	// Chmod failure -> Open error
	if _, err := Open("/dev/null"); err == nil {
		t.Error("Open(/dev/null) succeeded; want error on chmod failure")
	}
}
