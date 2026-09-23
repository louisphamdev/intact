package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The live database already holds errors written by the old schema. Opening it
// must add the new column, keep the old rows readable, and do nothing the
// second time.
func TestOpenAddsTheClientKeyColumnToAnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE upstream_errors (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		at          TEXT NOT NULL,
		provider    TEXT NOT NULL,
		connection  TEXT NOT NULL DEFAULT '',
		model       TEXT NOT NULL DEFAULT '',
		client      TEXT NOT NULL DEFAULT '',
		endpoint    TEXT NOT NULL DEFAULT '',
		status      INTEGER NOT NULL,
		latency_ms  INTEGER NOT NULL DEFAULT 0,
		class       TEXT NOT NULL DEFAULT '',
		signature   TEXT NOT NULL DEFAULT '',
		message     TEXT NOT NULL DEFAULT '',
		quota_left  REAL NOT NULL DEFAULT -1,
		headers     TEXT NOT NULL DEFAULT '',
		resp_body   TEXT NOT NULL DEFAULT '',
		req_body    TEXT NOT NULL DEFAULT ''
	);`); err != nil {
		t.Fatalf("old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO upstream_errors (at, provider, client, status, message, req_body)
		VALUES ('2026-01-01T00:00:00Z', 'groq', 'old-key', 500, 'old row', 'old body')`); err != nil {
		t.Fatalf("old row: %v", err)
	}
	db.Close()

	for i := range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		list, err := s.ListUpstreamErrors(ErrorFilter{})
		if err != nil || len(list) != 1 || list[0].Client != "old-key" || list[0].ClientKeyID != "" {
			t.Fatalf("open %d: list = %+v, err = %v", i+1, list, err)
		}
		e, err := s.GetUpstreamError(list[0].ID)
		if err != nil || e.ReqBody != "old body" || e.ClientKeyID != "" {
			t.Fatalf("open %d: get = %+v, err = %v", i+1, e, err)
		}
		s.Close()
	}
}

// A new error keeps the key id of the caller that caused it.
func TestUpstreamErrorKeepsTheClientKeyID(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	id, err := s.AddUpstreamError(UpstreamError{Provider: "groq", Client: "key", ClientKeyID: "k1", Status: 400, ReqBody: "b"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	e, err := s.GetUpstreamError(id)
	if err != nil || e.ClientKeyID != "k1" {
		t.Fatalf("get = %+v, err = %v", e, err)
	}
	list, _ := s.ListUpstreamErrors(ErrorFilter{})
	if len(list) != 1 || list[0].ClientKeyID != "k1" {
		t.Fatalf("list = %+v", list)
	}
}
