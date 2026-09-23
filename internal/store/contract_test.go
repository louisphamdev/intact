package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestContractMigrationTwiceIsNoopAndReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")

	// Create an old-schema database before contract tables and before trusted column.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	oldSQL := `
	CREATE TABLE connections (
		id TEXT PRIMARY KEY, provider TEXT NOT NULL, label TEXT NOT NULL, secret TEXT NOT NULL,
		is_active INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	);
	CREATE TABLE api_keys (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, key TEXT NOT NULL UNIQUE, enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL
	);
	INSERT INTO api_keys (id, name, key, enabled, created_at) VALUES ('k1', 'test key', 'sk-intact-test', 1, '2026-01-01T00:00:00Z');
	`
	if _, err := db.Exec(oldSQL); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// First open: should migrate cleanly.
	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first open failed: %v", err)
	}
	keys, err := s1.ListAPIKeys()
	if err != nil {
		t.Fatalf("list api keys: %v", err)
	}
	if len(keys) != 1 || keys[0].Trusted {
		t.Fatalf("expected key to default to trusted=false, got %+v", keys[0])
	}
	s1.Close()

	// Second open: should be a no-op and succeed without errors.
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	defer s2.Close()

	// Check that contract tables exist and work.
	key, err := s2.GetContractKey()
	if err != nil || len(key) != 32 {
		t.Fatalf("contract key failed: err=%v len=%d", err, len(key))
	}
	key2, _ := s2.GetContractKey()
	if string(key) != string(key2) {
		t.Fatalf("contract key changed on second read: %x != %x", key, key2)
	}
}

func TestContractPruneKeepsExemptTrace(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prune.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	const model = "test-model"

	// Insert 250 trusted traces & shapes for test-model.
	for i := 1; i <= 250; i++ {
		traceID := fmtTraceID(i)
		created := now.Add(time.Duration(-300+i) * time.Minute)
		if err := s.InsertContractTrace(ContractTrace{
			ID:             traceID,
			Trusted:        true,
			Created:        created.UnixMilli(),
			Model:          model,
			Direction:      "request",
			Status:         "done",
			ReducerVersion: 1,
		}); err != nil {
			t.Fatalf("insert trace %d: %v", i, err)
		}
		if err := s.InsertContractShape(traceID, "intact", "request", `{"test": true}`, created.UnixMilli()); err != nil {
			t.Fatalf("insert shape %d: %v", i, err)
		}
	}

	// Finding with exempt trace pointing to trace 5 (which is an older trace that would normally be pruned).
	exemptTraceID := fmtTraceID(5)
	finding := ContractFinding{
		ID:                "finding-1",
		Model:             model,
		ClientFormat:      "anthropic",
		Direction:         "request",
		Path:              "messages[].content[].lost_field",
		Class:             "lost",
		FirstTrace:        fmtTraceID(1),
		FirstTraceTrusted: true,
		LastTrace:         fmtTraceID(250),
		ExemptTrace:       exemptTraceID,
		Status:            "open",
	}
	if err := s.UpsertContractFinding(finding); err != nil {
		t.Fatalf("upsert finding: %v", err)
	}

	// Run Prune.
	if err := s.PruneContracts(now, 1); err != nil {
		t.Fatalf("prune failed: %v", err)
	}

	// Verify that exempt trace shape is STILL returned!
	rec, err := s.GetContractShape(exemptTraceID, "intact", "request")
	if err != nil {
		t.Fatalf("exempt trace shape was pruned: %v", err)
	}
	if rec == "" {
		t.Fatal("exempt trace shape is empty")
	}

	// Verify that shapes count for model was trimmed towards the 200 limit (plus exempt traces).
	count, err := s.CountContractShapes(model, "request")
	if err != nil {
		t.Fatal(err)
	}
	if count > 201 {
		t.Fatalf("expected <= 201 shapes after prune, got %d", count)
	}
}

func TestContractReset(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reset.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Insert learned and signatures with key_id 'key-1' and reducer_version 1
	if err := s.UpsertContractLearned(ContractLearned{
		Kind:           "model",
		Subject:        "m1",
		Direction:      "request",
		Half:           "intact",
		Format:         "openai",
		Event:          "",
		Path:           "test.path",
		Type:           "string",
		Seen:           1,
		KeyID:          "key-1",
		ReducerVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	learned, _ := s.ListContractLearned("model", "m1")
	if len(learned) != 1 {
		t.Fatalf("expected 1 learned row, got %d", len(learned))
	}

	// Reset by key
	if err := s.ResetContract("key-1", 0); err != nil {
		t.Fatal(err)
	}
	learned, _ = s.ListContractLearned("model", "m1")
	if len(learned) != 0 {
		t.Fatalf("expected 0 learned rows after reset, got %d", len(learned))
	}
}

func fmtTraceID(i int) string {
	s := "trace-id-"
	for len(s)+len(string(rune('0'+i%10))) < 20 {
		s += "0"
	}
	return s + time.Duration(i).String()
}

func TestC16PruneContracts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "prune_c16.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC()
	const model = "test-model-c16"

	// 1. (b) 250 trusted two-half traces
	for i := 1; i <= 250; i++ {
		traceID := fmtTraceID(1000 + i)
		created := now.Add(time.Duration(-300+i) * time.Minute)
		if err := s.InsertContractTrace(ContractTrace{
			ID:             traceID,
			Trusted:        true,
			Created:        created.UnixMilli(),
			Model:          model,
			Direction:      "request",
			Status:         "done",
			ReducerVersion: 2,
		}); err != nil {
			t.Fatalf("insert trace %d: %v", i, err)
		}
		if err := s.InsertContractShape(traceID, "intact", "request", `{"half": "intact"}`, created.UnixMilli()); err != nil {
			t.Fatalf("insert intact shape %d: %v", i, err)
		}
		if err := s.InsertContractShape(traceID, "switcher", "request", `{"half": "switcher"}`, created.UnixMilli()); err != nil {
			t.Fatalf("insert switcher shape %d: %v", i, err)
		}
	}

	exemptTraceID := fmtTraceID(1005)
	finding := ContractFinding{
		ID:                "finding-c16-open",
		Model:             model,
		ClientFormat:      "anthropic",
		Direction:         "request",
		Path:              "messages[].content[].lost_field",
		Class:             "lost",
		FirstTrace:        fmtTraceID(1001),
		FirstTraceTrusted: true,
		LastTrace:         fmtTraceID(1250),
		ExemptTrace:       exemptTraceID,
		Status:            "open",
	}
	if err := s.UpsertContractFinding(finding); err != nil {
		t.Fatalf("upsert finding: %v", err)
	}

	// 2. (c) Parameterized reducer version: learned rows with v1 and v2
	_ = s.UpsertContractLearned(ContractLearned{
		Kind: "model", Subject: model, Direction: "request", Half: "intact", Format: "anthropic", Path: "v1.path", Type: "string", ReducerVersion: 1,
	})
	_ = s.UpsertContractLearned(ContractLearned{
		Kind: "model", Subject: model, Direction: "request", Half: "intact", Format: "anthropic", Path: "v2.path", Type: "string", ReducerVersion: 2,
	})

	// 3. (d) Expired finding older than 90 days
	oldTime := now.Add(-95 * 24 * time.Hour).UnixMilli()
	if err := s.UpsertContractFinding(ContractFinding{
		ID:             "finding-c16-exp",
		Model:          model,
		ClientFormat:   "anthropic",
		Direction:      "request",
		Path:           "old.path",
		Class:          "lost",
		Status:         "open",
		FirstTrace:     "trace-c16-exp-1",
		CreatedAt:      oldTime,
		UpdatedAt:      oldTime,
		ReducerVersion: 2,
	}); err != nil {
		t.Fatalf("insert expired finding: %v", err)
	}

	// 4. (e) Golden shapes: 80 traces (160 shapes) for claude, 80 traces (160 shapes) for codex
	for i := 1; i <= 80; i++ {
		trClaude := fmtTraceID(2000 + i)
		_ = s.InsertContractTrace(ContractTrace{
			ID: trClaude, Trusted: true, Created: now.UnixMilli(), Tool: "claude", Direction: "response", Status: "done", ReducerVersion: 2,
		})
		_ = s.InsertContractShape(trClaude, "client", "response", `{}`, now.UnixMilli())
		_ = s.InsertContractShape(trClaude, "upstream", "response", `{}`, now.UnixMilli())

		trCodex := fmtTraceID(3000 + i)
		_ = s.InsertContractTrace(ContractTrace{
			ID: trCodex, Trusted: true, Created: now.UnixMilli(), Tool: "codex", Direction: "response", Status: "done", ReducerVersion: 2,
		})
		_ = s.InsertContractShape(trCodex, "client", "response", `{}`, now.UnixMilli())
		_ = s.InsertContractShape(trCodex, "upstream", "response", `{}`, now.UnixMilli())
	}

	// Run PruneContracts with currentReducerVersion = 2
	if err := s.PruneContracts(now, 2); err != nil {
		t.Fatalf("prune failed: %v", err)
	}

	// Verify (b): every (trace_id, direction) keeps 0 or 2 shape rows
	rows, err := s.DB.Query(`SELECT trace_id, direction, count(*) FROM contract_shapes GROUP BY trace_id, direction`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tr, dir string
		var cnt int
		if err := rows.Scan(&tr, &dir, &cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 2 {
			t.Fatalf("trace %s direction %s has %d shapes, expected 0 or 2", tr, dir, cnt)
		}
	}

	// Verify exempt trace still has both shapes
	shapeIntact, err := s.GetContractShape(exemptTraceID, "intact", "request")
	if err != nil || shapeIntact == "" {
		t.Fatalf("exempt trace intact shape missing: %v", err)
	}
	shapeSwitcher, err := s.GetContractShape(exemptTraceID, "switcher", "request")
	if err != nil || shapeSwitcher == "" {
		t.Fatalf("exempt trace switcher shape missing: %v", err)
	}

	// Verify (c): reducer_version = 2 rows kept, reducer_version = 1 deleted
	learned, err := s.ListContractLearned("model", model)
	if err != nil {
		t.Fatal(err)
	}
	if len(learned) != 1 || learned[0].Path != "v2.path" {
		t.Fatalf("expected only v2.path kept, got %+v", learned)
	}

	// Verify (d): expired finding got history row who='system' open->expired
	fExp, err := s.GetContractFinding("finding-c16-exp")
	if err != nil {
		t.Fatal(err)
	}
	if fExp.Status != "expired" {
		t.Fatalf("expected status expired, got %s", fExp.Status)
	}
	hist, err := s.ListFindingHistory("finding-c16-exp")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Who != "system" || hist[0].OldStatus != "open" || hist[0].NewStatus != "expired" {
		t.Fatalf("expected 1 history row who=system open->expired, got %+v", hist)
	}

	// Verify (e): golden shapes counted per tool (all 160 claude and 160 codex shapes kept)
	var claudeCount, codexCount int
	_ = s.DB.QueryRow(`SELECT count(*) FROM contract_shapes s JOIN contract_traces t ON s.trace_id = t.id WHERE t.tool = 'claude'`).Scan(&claudeCount)
	_ = s.DB.QueryRow(`SELECT count(*) FROM contract_shapes s JOIN contract_traces t ON s.trace_id = t.id WHERE t.tool = 'codex'`).Scan(&codexCount)
	if claudeCount != 160 {
		t.Fatalf("expected 160 claude golden shapes, got %d", claudeCount)
	}
	if codexCount != 160 {
		t.Fatalf("expected 160 codex golden shapes, got %d", codexCount)
	}
}

func TestC17ResetContractBothFlagsRefused(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reset_c17.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.UpsertContractLearned(ContractLearned{
		Kind: "model", Subject: "m1", Direction: "request", Half: "intact", Format: "openai", Path: "p1", Type: "string", KeyID: "k", ReducerVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}

	err = s.ResetContract("k", 1)
	if err == nil {
		t.Fatal("expected error when both key and reducer version specified, got nil")
	}

	learned, _ := s.ListContractLearned("model", "m1")
	if len(learned) != 1 {
		t.Fatalf("expected learned row to be preserved, got %d", len(learned))
	}
}
