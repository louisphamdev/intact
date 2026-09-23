package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

func TestJudge_JudgeOnce(t *testing.T) {
	s := newTestStore(t)
	stub := NewStubJudge(map[string]string{
		"missing_field": "lost",
	})
	jm := NewJudgeManager(s, stub)

	cand := Candidate{
		Model:        "model-a",
		ClientFormat: "anthropic",
		Direction:    "request",
		Path:         "missing_field",
		Type:         "string",
	}

	// First time: candidate judged
	err := jm.ProcessCandidates(context.Background(), "key-1", "trace-1", []Candidate{cand}, nil, nil, nil)
	if err != nil {
		t.Fatalf("first process: %v", err)
	}
	if stub.Calls != 1 {
		t.Fatalf("expected 1 judge call, got %d", stub.Calls)
	}

	// Verify signature verdict is approved (since lost is resolved)
	sig, err := s.GetContractSignature(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
	if err != nil {
		t.Fatalf("get signature: %v", err)
	}
	if sig.Verdict != "lost" || sig.VerdictState != "approved" {
		t.Fatalf("expected lost/approved, got %s/%s", sig.Verdict, sig.VerdictState)
	}

	// Second time with same signature: should NOT judge again
	err = jm.ProcessCandidates(context.Background(), "key-1", "trace-2", []Candidate{cand}, nil, nil, nil)
	if err != nil {
		t.Fatalf("second process: %v", err)
	}
	if stub.Calls != 1 {
		t.Fatalf("expected judge calls to remain 1, got %d", stub.Calls)
	}
}

func TestJudge_RejudgePending(t *testing.T) {
	s := newTestStore(t)
	stub := &StubJudge{
		Err: errors.New("timeout connecting to model"),
	}
	jm := NewJudgeManager(s, stub)

	cand := Candidate{
		Model:        "model-a",
		ClientFormat: "anthropic",
		Direction:    "request",
		Path:         "timeout_field",
		Type:         "string",
	}

	// First trace: judge errors
	_ = jm.ProcessCandidates(context.Background(), "key-1", "trace-1", []Candidate{cand}, nil, nil, nil)

	sig, err := s.GetContractSignature(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
	if err != nil {
		t.Fatalf("get signature: %v", err)
	}
	if sig.VerdictState != "pending" {
		t.Fatalf("expected pending state, got %s", sig.VerdictState)
	}
	if sig.ErrorCount != 1 {
		t.Fatalf("expected error count 1, got %d", sig.ErrorCount)
	}

	// Reset lease (simulating lease expiry or startup sweep)
	_, _ = s.DB.Exec(`UPDATE contract_signatures SET lease_until = 0 WHERE path = 'timeout_field'`)

	// Second trace: judge succeeds with lost
	stub.Err = nil
	stub.Answers = map[string]string{"timeout_field": "lost"}

	// Insert trace-2 so trace exists for finding creation
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:           "trace-2",
		KeyID:        "key-1",
		Trusted:      true,
		Model:        "model-a",
		ClientFormat: "anthropic",
		Direction:    "request",
		Status:       "queued",
	})

	err = jm.ProcessCandidates(context.Background(), "key-1", "trace-2", []Candidate{cand}, nil, nil, nil)
	if err != nil {
		t.Fatalf("second process: %v", err)
	}

	// Signature must now be approved
	sig, _ = s.GetContractSignature(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
	if sig.Verdict != "lost" || sig.VerdictState != "approved" {
		t.Fatalf("expected lost/approved, got %s/%s", sig.Verdict, sig.VerdictState)
	}

	// Exactly one finding opened
	findings, err := s.ListContractFindings("open", 0, true)
	if err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding, got %d", len(findings))
	}
	if findings[0].Path != "timeout_field" {
		t.Fatalf("unexpected finding path: %s", findings[0].Path)
	}
}

func TestJudge_AnswerValidation(t *testing.T) {
	s := newTestStore(t)
	stub := &StubJudge{
		Answers: map[string]string{
			"bad_field": "renamed:99", // 99 is out of range
		},
	}
	jm := NewJudgeManager(s, stub)

	cand := Candidate{
		Model:        "model-b",
		ClientFormat: "anthropic",
		Direction:    "response",
		Path:         "bad_field",
		Type:         "string",
	}

	// First invalid answer
	_ = jm.ProcessCandidates(context.Background(), "key-1", "trace-1", []Candidate{cand}, []string{"out1", "out2"}, nil, nil)
	sig, _ := s.GetContractSignature(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
	if sig.VerdictState != "pending" || sig.ErrorCount != 1 {
		t.Fatalf("expected pending/1 error, got %s/%d", sig.VerdictState, sig.ErrorCount)
	}

	// Second invalid answer
	_, _ = s.DB.Exec(`UPDATE contract_signatures SET lease_until = 0 WHERE path = 'bad_field'`)
	_ = jm.ProcessCandidates(context.Background(), "key-1", "trace-1", []Candidate{cand}, []string{"out1", "out2"}, nil, nil)
	sig, _ = s.GetContractSignature(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
	if sig.VerdictState != "pending" || sig.ErrorCount != 2 {
		t.Fatalf("expected pending/2 errors, got %s/%d", sig.VerdictState, sig.ErrorCount)
	}

	// Third invalid answer -> transitions to error
	_, _ = s.DB.Exec(`UPDATE contract_signatures SET lease_until = 0 WHERE path = 'bad_field'`)
	_ = jm.ProcessCandidates(context.Background(), "key-1", "trace-1", []Candidate{cand}, []string{"out1", "out2"}, nil, nil)
	sig, _ = s.GetContractSignature(cand.Model, cand.ClientFormat, cand.Direction, cand.Path, cand.Type)
	if sig.VerdictState != "error" || sig.ErrorCount != 3 {
		t.Fatalf("expected error state after 3 errors, got %s/%d", sig.VerdictState, sig.ErrorCount)
	}
}

func TestConsumer_TrustOverTime(t *testing.T) {
	// Spec section 14:
	// "Trust over time: a key demoted between capture and half creates no learned row, no judge call and
	// no finding. An untrusted half with a greater switcherVersion leaves a fixed finding fixed."
	s := newTestStore(t)
	stub := NewStubJudge(map[string]string{"demoted_field": "lost"})
	jm := NewJudgeManager(s, stub)
	consumer := NewConsumer(s, jm, nil)

	// Pre-seed a 'fixed' finding
	findingID := NewFindingID()
	now := time.Now().UnixMilli()
	_ = s.UpsertContractFinding(store.ContractFinding{
		ID:                findingID,
		Model:             "model-x",
		ClientFormat:      "anthropic",
		Direction:         "request",
		Path:              "demoted_field",
		Class:             "lost",
		Status:            "fixed",
		FixedIn:           "1.0.0+20260923T100000Z",
		FirstTrace:        "initial-trace",
		FirstTraceTrusted: true,
		LastTrace:         "initial-trace",
		CreatedAt:         now,
		UpdatedAt:         now,
	})

	// Process trace with Trusted = false
	untrustedTraceID := "untrusted-trace-1"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              untrustedTraceID,
		KeyID:           "demoted-key",
		Trusted:         false, // demoted!
		Model:           "model-x",
		ClientFormat:    "anthropic",
		SwitcherVersion: "99.0.0+20260923T120000Z", // greater version
		Status:          "queued",
	})

	// Shape for untrusted trace
	rec := ShapeRecord{
		ReducerVersion: ReducerVersion,
		Records: []Record{
			{Leaves: []Leaf{{Path: "demoted_field", Type: "string", Len: 5}}},
		},
	}
	raw, _ := json.Marshal(rec)
	_ = s.InsertContractShape(untrustedTraceID, "switcher", "request", string(raw), now)
	_ = s.InsertContractShape(untrustedTraceID, "intact", "request", `{"reducerVersion":1,"records":[]}`, now)

	consumer.ProcessTraceSync(untrustedTraceID)

	// 1. Stub judge must NOT have been called
	if stub.Calls != 0 {
		t.Fatalf("expected 0 judge calls for untrusted trace, got %d", stub.Calls)
	}

	// 2. No learned rows created
	learned, _ := s.ListContractLearned("model", "model-x")
	if len(learned) != 0 {
		t.Fatalf("expected 0 learned rows for untrusted trace, got %d", len(learned))
	}

	// 3. Fixed finding must REMAIN fixed
	f, err := s.GetContractFinding(findingID)
	if err != nil {
		t.Fatalf("get finding: %v", err)
	}
	if f.Status != "fixed" {
		t.Fatalf("expected finding status to remain 'fixed', got '%s'", f.Status)
	}
}

func TestFindings_KeysAndReducerVersion(t *testing.T) {
	// Spec section 14:
	// "Keys: a session-set wontfix stays wontfix after the reducer version changes, and no new open row
	// appears for its path. The same upstream path lost by the anthropic converter and kept by the
	// responses converter gives exactly one finding, tagged anthropic."
	s := newTestStore(t)
	stub := NewStubJudge(map[string]string{"wontfix_field": "lost"})
	jm := NewJudgeManager(s, stub)
	consumer := NewConsumer(s, jm, nil)

	findingID := NewFindingID()
	now := time.Now().UnixMilli()
	_ = s.UpsertContractFinding(store.ContractFinding{
		ID:                findingID,
		Model:             "model-keys",
		ClientFormat:      "anthropic",
		Direction:         "request",
		Path:              "wontfix_field",
		Class:             "lost",
		Status:            "wontfix",
		FirstTrace:        "trace-wf",
		FirstTraceTrusted: true,
		LastTrace:         "trace-wf",
		CreatedAt:         now,
		UpdatedAt:         now,
	})
	// Log history that session set it
	_, _ = s.DB.Exec(`INSERT INTO contract_finding_history (finding_id, at, who, old_status, new_status, note)
		VALUES (?, ?, 'session', 'open', 'wontfix', 'session marked wontfix')`, findingID, now)

	// Process a new trusted trace with higher version and new reducer
	traceID := "trace-new-reducer"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              traceID,
		KeyID:           "session-key",
		Trusted:         true,
		Model:           "model-keys",
		ClientFormat:    "anthropic",
		SwitcherVersion: "2.0.0+20260923T120000Z",
		Status:          "queued",
		ReducerVersion:  2,
	})

	rec := ShapeRecord{
		ReducerVersion: 2,
		Records: []Record{
			{Leaves: []Leaf{{Path: "wontfix_field", Type: "string", Len: 5}}},
		},
	}
	raw, _ := json.Marshal(rec)
	_ = s.InsertContractShape(traceID, "switcher", "request", string(raw), now)
	_ = s.InsertContractShape(traceID, "intact", "request", `{"reducerVersion":2,"records":[]}`, now)

	consumer.ProcessTraceSync(traceID)

	// Finding must remain wontfix!
	f, _ := s.GetContractFinding(findingID)
	if f.Status != "wontfix" {
		t.Fatalf("expected session wontfix to remain 'wontfix', got '%s'", f.Status)
	}

	// No new open finding created for the same path
	allFindings, _ := s.ListContractFindings("", 0, true)
	if len(allFindings) != 1 {
		t.Fatalf("expected exactly 1 finding row, got %d", len(allFindings))
	}
}

func TestFindings_FixedIn_Resolution(t *testing.T) {
	// Spec section 14:
	// "fixedIn: an untrusted half with version 99999.0.0+... uploaded before the resolve leaves fixedIn
	// at the greatest trusted version. A version whose time part is 99999999T999999Z gets 400."
	s := newTestStore(t)

	// Insert trusted trace with version 1.5.0
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              "trace-trusted-1",
		Model:           "model-res",
		Trusted:         true,
		SwitcherVersion: "1.5.0+20260923T100000Z",
		Status:          "done",
	})

	// Insert untrusted trace with huge version 99999.0.0
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              "trace-untrusted-huge",
		Model:           "model-res",
		Trusted:         false,
		SwitcherVersion: "99999.0.0+20260923T110000Z",
		Status:          "done",
	})

	findingID := NewFindingID()
	now := time.Now().UnixMilli()
	_ = s.UpsertContractFinding(store.ContractFinding{
		ID:                findingID,
		Model:             "model-res",
		ClientFormat:      "anthropic",
		Direction:         "request",
		Path:              "res_path",
		Class:             "lost",
		Status:            "open",
		FirstTrace:        "trace-trusted-1",
		FirstTraceTrusted: true,
		LastTrace:         "trace-trusted-1",
		CreatedAt:         now,
		UpdatedAt:         now,
	})

	// Resolve as fixed
	err := ResolveFinding(s, findingID, "fixed", "key-admin", "resolved")
	if err != nil {
		t.Fatalf("resolve finding: %v", err)
	}

	f, _ := s.GetContractFinding(findingID)
	if f.Status != "fixed" {
		t.Fatalf("expected status 'fixed', got '%s'", f.Status)
	}
	// fixedIn must be 1.5.0, not the untrusted 99999.0.0
	if f.FixedIn != "1.5.0+20260923T100000Z" {
		t.Fatalf("expected fixedIn '1.5.0+20260923T100000Z', got '%s'", f.FixedIn)
	}

	// ValidateSwitcherVersion bounds check on 99999999T999999Z
	err = ValidateSwitcherVersion("1.0.0+99999999T999999Z", time.Now().UTC())
	if err == nil {
		t.Fatal("expected ValidateSwitcherVersion to reject invalid time part 99999999T999999Z")
	}
}

func TestConsumer_QueueDropCounters(t *testing.T) {
	// Spec section 8 & 14:
	// "All of this runs on one consumer goroutine per intact process, fed by a queue of 256 sealed traces.
	// When the queue is full, a trace is marked dropped, counted on the dashboard, and excluded from every counter."
	s := newTestStore(t)
	consumer := NewConsumer(s, nil, nil)
	// Do not start consumer so queue fills up to 256

	for i := 0; i < 256; i++ {
		id := "queued-trace-" + string(rune('a'+(i%26))) + string(rune('0'+(i/26)))
		_ = s.InsertContractTrace(store.ContractTrace{ID: id, Status: "queued"})
		ok := consumer.Enqueue(id)
		if !ok {
			t.Fatalf("expected first 256 items to enqueue successfully, failed at %d", i)
		}
	}

	// 257th trace: queue is full
	dropID := "dropped-trace-overflow"
	_ = s.InsertContractTrace(store.ContractTrace{ID: dropID, Status: "queued"})
	ok := consumer.Enqueue(dropID)
	if ok {
		t.Fatal("expected 257th trace enqueue to fail because queue is full")
	}

	if consumer.DroppedCount() != 1 {
		t.Fatalf("expected dropped count 1, got %d", consumer.DroppedCount())
	}

	// Trace in DB must be updated to 'dropped'
	tr, err := s.GetContractTrace(dropID)
	if err != nil {
		t.Fatalf("get trace: %v", err)
	}
	if tr.Status != "dropped" {
		t.Fatalf("expected trace status 'dropped', got '%s'", tr.Status)
	}

	// Counter in DB must be 1
	counter, _ := s.GetContractCounter("dropped_traces")
	if counter != 1 {
		t.Fatalf("expected persistent dropped counter 1, got %d", counter)
	}
}

func TestChangeDetection_AlertText(t *testing.T) {
	// Spec section 8:
	// "One alert per tool or model per 6 hours goes to the notify channels. The alert text holds a count
	// and a finding id, never a path or a version string."
	findingID := "fnd_1234567890abcdef"
	alert := FormatChangeAlert(5, findingID)

	if !strings.Contains(alert, "5") {
		t.Fatalf("alert text must contain count: %s", alert)
	}
	if !strings.Contains(alert, findingID) {
		t.Fatalf("alert text must contain finding ID: %s", alert)
	}

	// Must never contain sensitive info
	forbiddenSubstrings := []string{
		"choices[].delta",
		"messages[0].content",
		"/v1/messages",
		"2.10.0",
		"v1.0.0",
	}
	for _, forbidden := range forbiddenSubstrings {
		if strings.Contains(alert, forbidden) {
			t.Fatalf("alert text must NOT contain path or version string ('%s'): %s", forbidden, alert)
		}
	}
}

func TestC8UntrustedQueueRelease(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)
	consumer := NewConsumer(s, nil, nil)
	m.SetEnqueue(func(traceID string) {
		consumer.ProcessTraceSync(traceID)
	})
	consumer.SetReleaseQueueSlot(m.ReleaseQueueSlot)

	keyID := "untrusted-key-c8"
	for i := 0; i < 20; i++ {
		traceID := fmt.Sprintf("trace-c8-%04d-12345678", i)
		cap, ok := m.ClaimTraced(traceID, keyID, false, "model-1", "anthropic", "v1/messages")
		if !ok || cap == nil {
			t.Fatalf("trace %d: claim failed", i)
		}
		cap.WriteReq([]byte(`{"model":"model-1","messages":[{"role":"user","content":"hi"}]}`))
		cap.WriteResp([]byte(`{"content":[{"type":"text","text":"hello"}]}`))
		close(cap.ProducerDone)
		if err := cap.Seal(false, nil, nil); err != nil {
			t.Fatalf("trace %d: seal failed: %v", i, err)
		}

		half := &HalfPayload{
			ToolRequest:     `{"model":"model-1","input":"hi"}`,
			ToolResponse:    `{"output":"hello"}`,
			ToolVersion:     "1.0.0",
			SwitcherVersion: "1.0.0+20260924T000000Z",
			Converter: ConverterFormat{
				InFormat:  "anthropic",
				OutFormat: "anthropic",
			},
		}

		code, err := m.SubmitHalf(traceID, keyID, false, half)
		if code != http.StatusOK {
			t.Fatalf("trace %d: submit half failed with code %d: %v", i, code, err)
		}

		tr, err := s.GetContractTrace(traceID)
		if err != nil {
			t.Fatalf("trace %d: get trace failed: %v", i, err)
		}
		if tr.Status == "dropped" {
			t.Fatalf("trace %d: dropped! queue count: %d", i, m.QueueCount(keyID))
		}
		if tr.Status != "done" {
			t.Fatalf("trace %d: unexpected status %s, want done", i, tr.Status)
		}
	}

	if q := m.QueueCount(keyID); q != 0 {
		t.Fatalf("expected queue count 0, got %d", q)
	}
}

func TestC4KeyDemotedWhileCapturingAndQueued(t *testing.T) {
	s := newTestStore(t)
	stub := NewStubJudge(map[string]string{"new_field": "lost"})
	jm := NewJudgeManager(s, stub)
	consumer := NewConsumer(s, jm, nil)

	countRows := func() (int, int, int) {
		var l, sig, f int
		_ = s.DB.QueryRow(`SELECT count(*) FROM contract_learned`).Scan(&l)
		_ = s.DB.QueryRow(`SELECT count(*) FROM contract_signatures`).Scan(&sig)
		_ = s.DB.QueryRow(`SELECT count(*) FROM contract_findings`).Scan(&f)
		return l, sig, f
	}

	half := &HalfPayload{
		ToolRequest:     `{"model":"model-c4","input":"hi"}`,
		ToolResponse:    `{"output":"hello"}`,
		ToolVersion:     "1.0.0",
		SwitcherVersion: "1.0.0+20260924T000000Z",
		Converter: ConverterFormat{
			InFormat:  "anthropic",
			OutFormat: "anthropic",
		},
	}

	// 1. Key demoted while trace capturing
	k1, err := s.CreateAPIKey("k1")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetAPIKeyTrusted(k1.ID, true)
	m1 := NewCaptureManager(s, testKey)

	trace1 := "trace-c4-cap-12345678"
	c1, ok := m1.ClaimTraced(trace1, k1.ID, true, "model-c4", "anthropic", "v1/messages")
	if !ok {
		t.Fatal("claim 1 failed")
	}
	c1.WriteReq([]byte(`{"model":"model-c4","messages":[{"role":"user","content":"hi"}]}`))
	c1.WriteResp([]byte(`{"content":[{"type":"text","text":"hello"}]}`))
	close(c1.ProducerDone)

	// Demote k1 while capturing
	_ = s.SetAPIKeyTrusted(k1.ID, false)

	_ = c1.Seal(false, nil, nil)
	code, err := m1.SubmitHalf(trace1, k1.ID, false, half)
	if code != http.StatusOK && code != http.StatusAccepted {
		t.Fatalf("submit 1 failed: code %d, err %v", code, err)
	}
	consumer.ProcessTraceSync(trace1)

	l, sig, f := countRows()
	if l != 0 || sig != 0 || f != 0 {
		t.Fatalf("demoted while capturing produced rows: learned=%d sig=%d findings=%d", l, sig, f)
	}

	// 2. Key demoted while queued
	k2, err := s.CreateAPIKey("k2")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetAPIKeyTrusted(k2.ID, true)
	m2 := NewCaptureManager(s, testKey)

	trace2 := "trace-c4-que-12345678"
	c2, ok := m2.ClaimTraced(trace2, k2.ID, true, "model-c4", "anthropic", "v1/messages")
	if !ok {
		t.Fatal("claim 2 failed")
	}
	c2.WriteReq([]byte(`{"model":"model-c4","messages":[{"role":"user","content":"hi"}]}`))
	c2.WriteResp([]byte(`{"content":[{"type":"text","text":"hello"}]}`))
	close(c2.ProducerDone)
	_ = c2.Seal(false, nil, nil)

	code, err = m2.SubmitHalf(trace2, k2.ID, true, half)
	if code != http.StatusOK {
		t.Fatalf("submit 2 failed: code %d, err %v", code, err)
	}

	// Demote k2 while queued
	_ = s.SetAPIKeyTrusted(k2.ID, false)

	consumer.ProcessTraceSync(trace2)

	l, sig, f = countRows()
	if l != 0 || sig != 0 || f != 0 {
		t.Fatalf("demoted while queued produced rows: learned=%d sig=%d findings=%d", l, sig, f)
	}

	// 3. Master token (keyID == "") remains trusted
	m3 := NewCaptureManager(s, testKey)
	trace3 := "trace-c4-mas-12345678"
	c3, ok := m3.ClaimTraced(trace3, "", true, "model-c4", "anthropic", "v1/messages")
	if !ok {
		t.Fatal("claim 3 failed")
	}
	c3.WriteReq([]byte(`{"model":"model-c4","messages":[{"role":"user","content":"hi"}]}`))
	c3.WriteResp([]byte(`{"content":[{"type":"text","text":"hello"}]}`))
	close(c3.ProducerDone)
	_ = c3.Seal(false, nil, nil)
	code, err = m3.SubmitHalf(trace3, "", true, half)
	if code != http.StatusOK {
		t.Fatalf("submit 3 failed: code %d, err %v", code, err)
	}

	consumer.ProcessTraceSync(trace3)

	l, _, _ = countRows()
	if l == 0 {
		t.Fatal("master token trace should produce learned rows")
	}
}

func TestC12TruncatedRecordsHandling(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)

	// 1. Verify request over 4 MiB puts {cut} in reqShape only
	traceID := "trace-c12-req-cut-123456"
	cap, ok := m.ClaimTraced(traceID, "key-12", true, "model-c12", "anthropic", "v1/messages")
	if !ok || cap == nil {
		t.Fatal("claim failed")
	}
	// Write > 4 MiB to ReqBuf
	bigBuf := make([]byte, MaxHalfBytes+100)
	cap.WriteReq(bigBuf)
	cap.WriteResp([]byte(`{"status":"ok"}`))
	close(cap.ProducerDone)
	if err := cap.Seal(false, nil, nil); err != nil {
		t.Fatalf("seal failed: %v", err)
	}

	shapes, err := s.ListContractShapesForTrace(traceID)
	if err != nil {
		t.Fatal(err)
	}
	var reqHasCut, respHasCut bool
	for _, shape := range shapes {
		if shape.Direction == "request" && strings.Contains(shape.Record, `"{cut}"`) {
			reqHasCut = true
		}
		if shape.Direction == "response" && strings.Contains(shape.Record, `"{cut}"`) {
			respHasCut = true
		}
	}
	if !reqHasCut {
		t.Fatal("expected reqShape to contain {cut}")
	}
	if respHasCut {
		t.Fatal("expected respShape to NOT contain {cut} when only request was truncated")
	}

	// 2. Record with cut leaf -> no UpsertContractLearned, no gone change, no alert
	var alertCalled bool
	alertFn := func(model, tool, text string) {
		alertCalled = true
	}
	consumer := NewConsumer(s, nil, alertFn)

	k, err := s.CreateAPIKey("k-c12")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetAPIKeyTrusted(k.ID, true)

	// Seed model with a known path so gone detection could theoretically trigger
	traceSeed := "trace-c12-seed-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:           traceSeed,
		KeyID:        k.ID,
		Trusted:      true,
		Model:        "model-c12",
		ClientFormat: "anthropic",
		Status:       "queued",
	})
	_ = s.InsertContractShape(traceSeed, "intact", "response", `{"reducerVersion":1,"records":[{"leaves":[{"path":"data.id","type":"string"}]}]}`, time.Now().UnixMilli())
	consumer.ProcessTraceSync(traceSeed)

	// Count learned rows before
	var learnedBefore int
	_ = s.DB.QueryRow(`SELECT count(*) FROM contract_learned WHERE subject = 'model-c12'`).Scan(&learnedBefore)
	if learnedBefore == 0 {
		t.Fatal("expected seed trace to be learned")
	}

	// Now send 20 traces with a cut leaf (which do NOT contain data.id and contain new_field)
	for i := 0; i < 20; i++ {
		tID := fmt.Sprintf("trace-c12-cut-%02d-123456", i)
		_ = s.InsertContractTrace(store.ContractTrace{
			ID:           tID,
			KeyID:        k.ID,
			Trusted:      true,
			Model:        "model-c12",
			ClientFormat: "anthropic",
			Status:       "queued",
		})
		_ = s.InsertContractShape(tID, "intact", "response", `{"reducerVersion":1,"records":[{"leaves":[{"path":"new_field","type":"string"},{"path":"{cut}","type":"cut"}]}]}`, time.Now().UnixMilli())
		consumer.ProcessTraceSync(tID)
	}

	// Verify no new UpsertContractLearned was written
	var learnedAfter int
	_ = s.DB.QueryRow(`SELECT count(*) FROM contract_learned WHERE subject = 'model-c12'`).Scan(&learnedAfter)
	if learnedAfter != learnedBefore {
		t.Fatalf("expected learned count to remain %d, got %d", learnedBefore, learnedAfter)
	}

	// Verify no gone change triggered alert or 1.0 policy
	if alertCalled {
		t.Fatal("expected no alert to be called for truncated records")
	}
	policy := consumer.GetPolicy()
	if rate, ok := policy.Models["model-c12"]; ok && rate == 1.0 {
		t.Fatal("expected model-c12 policy rate not to be 1.0 from gone changes on truncated records")
	}
}

func TestC11NewModelAndChangedModelPolicy(t *testing.T) {
	s := newTestStore(t)
	var alertCount int
	alertFn := func(model, tool, text string) {
		alertCount++
	}
	consumer := NewConsumer(s, nil, alertFn)

	k, err := s.CreateAPIKey("k-c11")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetAPIKeyTrusted(k.ID, true)

	// 1. First trusted trace of a new model with multiple leaves
	trace1 := "trace-c11-first-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:           trace1,
		KeyID:        k.ID,
		Trusted:      true,
		Model:        "model-c11",
		ClientFormat: "anthropic",
		Status:       "queued",
	})
	_ = s.InsertContractShape(trace1, "intact", "response", `{"reducerVersion":1,"records":[{"leaves":[{"path":"status","type":"string"},{"path":"code","type":"number"},{"path":"message","type":"string"}]}]}`, time.Now().UnixMilli())
	consumer.ProcessTraceSync(trace1)

	// First trusted trace of a new model must NOT alert and GetPolicy must NOT give 1.0
	if alertCount != 0 {
		t.Fatalf("first trace of new model called alertFn (count %d)", alertCount)
	}
	policy := consumer.GetPolicy()
	if rate, ok := policy.Models["model-c11"]; ok && rate == 1.0 {
		t.Fatal("first trace of new model caused policy 1.0")
	}

	// 2. Real change: Introduce a new path
	traceChange := "trace-c11-change-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:           traceChange,
		KeyID:        k.ID,
		Trusted:      true,
		Model:        "model-c11",
		ClientFormat: "anthropic",
		Status:       "queued",
	})
	_ = s.InsertContractShape(traceChange, "intact", "response", `{"reducerVersion":1,"records":[{"leaves":[{"path":"status","type":"string"},{"path":"code","type":"number"},{"path":"message","type":"string"},{"path":"new_feature","type":"string"}]}]}`, time.Now().UnixMilli())
	consumer.ProcessTraceSync(traceChange)

	policy = consumer.GetPolicy()
	if rate := policy.Models["model-c11"]; rate != 1.0 {
		t.Fatalf("expected policy 1.0 after real change, got %v", rate)
	}

	// 3. 20 more trusted traces return model to default rate
	for i := 0; i < 20; i++ {
		tID := fmt.Sprintf("trace-c11-post-%02d-123456", i)
		_ = s.InsertContractTrace(store.ContractTrace{
			ID:           tID,
			KeyID:        k.ID,
			Trusted:      true,
			Model:        "model-c11",
			ClientFormat: "anthropic",
			Status:       "queued",
		})
		_ = s.InsertContractShape(tID, "intact", "response", `{"reducerVersion":1,"records":[{"leaves":[{"path":"status","type":"string"},{"path":"code","type":"number"},{"path":"message","type":"string"},{"path":"new_feature","type":"string"}]}]}`, time.Now().UnixMilli())
		consumer.ProcessTraceSync(tID)
	}

	policy = consumer.GetPolicy()
	if rate, ok := policy.Models["model-c11"]; ok && rate == 1.0 {
		t.Fatalf("expected policy for model-c11 to return to default after 20 traces, but got %v", rate)
	}
}

func TestC13GoldenTraceLearning(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)
	consumer := NewConsumer(s, nil, nil)

	k, err := s.CreateAPIKey("k-c13")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetAPIKeyTrusted(k.ID, true)

	cap, ok := m.ClaimGolden(k.ID, true, "anthropic", "claude-cli/1.2.3 (darwin; arm64)", "/v1/messages")
	if !ok || cap == nil {
		t.Fatal("claim golden failed")
	}

	cap.WriteReq([]byte(`{"model":"claude-3-haiku-20240307","messages":[{"role":"user","content":"hello"}]}`))
	cap.WriteResp([]byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`))
	close(cap.ProducerDone)

	if err := cap.Seal(false, nil, nil); err != nil {
		t.Fatalf("seal golden failed: %v", err)
	}

	consumer.ProcessTraceSync(cap.TraceID)

	// Done when: golden claude trace through ProcessTraceSync reaches done without open; contract_learned has kind='tool' subject='claude'; 0 rows subject=''.
	tr, err := s.GetContractTrace(cap.TraceID)
	if err != nil {
		t.Fatalf("get trace: %v", err)
	}
	if tr.Status != "done" {
		t.Fatalf("expected trace status 'done', got '%s'", tr.Status)
	}

	openFindings, err := s.ListContractFindings("open", 0, true)
	if err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(openFindings) != 0 {
		t.Fatalf("expected 0 open findings, got %d", len(openFindings))
	}

	toolLearned, err := s.ListContractLearned("tool", "claude")
	if err != nil {
		t.Fatalf("list contract learned: %v", err)
	}
	if len(toolLearned) == 0 {
		t.Fatal("expected contract_learned to have rows for kind='tool' subject='claude'")
	}

	var emptySubjectCount int
	_ = s.DB.QueryRow(`SELECT count(*) FROM contract_learned WHERE subject = ''`).Scan(&emptySubjectCount)
	if emptySubjectCount != 0 {
		t.Fatalf("expected 0 rows with subject='', got %d", emptySubjectCount)
	}
}

func TestC14RenamedWorkflow(t *testing.T) {
	s := newTestStore(t)
	stub := NewStubJudge(map[string]string{
		"old_field": "renamed:0",
	})
	jm := NewJudgeManager(s, stub)
	consumer := NewConsumer(s, jm, nil)

	k, err := s.CreateAPIKey("k-c14")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetAPIKeyTrusted(k.ID, true)

	// Trace 1: toolRequest has old_field, intactReq has new_field
	trace1 := "trace-c14-1-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:           trace1,
		KeyID:        k.ID,
		Trusted:      true,
		Model:        "model-c14",
		ClientFormat: "anthropic",
		Status:       "queued",
	})
	_ = s.InsertContractShape(trace1, "switcher", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"old_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())
	_ = s.InsertContractShape(trace1, "intact", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"new_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())

	consumer.ProcessTraceSync(trace1)

	// Step 1: Check signature keeps target path
	sig, err := s.GetContractSignature("model-c14", "anthropic", "request", "old_field", "string")
	if err != nil {
		t.Fatalf("get signature: %v", err)
	}
	if !strings.Contains(sig.Verdict, "new_field") {
		t.Fatalf("expected signature verdict to keep target path 'new_field', got '%s'", sig.Verdict)
	}

	// Step 2: Approve the signature
	approvedSig, err := s.ReviewSignatureVerdict("model-c14", "anthropic", "request", "old_field", "string", "approve")
	if err != nil {
		t.Fatalf("review signature verdict: %v", err)
	}
	target := strings.TrimPrefix(approvedSig.Verdict, "renamed:")
	tr, _ := s.GetContractTrace(trace1)
	cand := Candidate{
		Model:        approvedSig.Model,
		ClientFormat: approvedSig.ClientFormat,
		Direction:    approvedSig.Direction,
		Path:         approvedSig.Path,
		Type:         approvedSig.Type,
	}
	_ = HandleLostCandidate(s, tr, cand, target)

	// Step 3: Check finding.mapping equals that path
	finding, err := s.GetContractFindingByPath("model-c14", "anthropic", "request", "old_field")
	if err != nil {
		t.Fatalf("get finding: %v", err)
	}
	if finding.Mapping != "new_field" {
		t.Fatalf("expected finding mapping to be 'new_field', got '%s'", finding.Mapping)
	}

	// Step 4: Next trusted trace with same rename -> Diff receives approved mapping and gives no candidate
	trace2 := "trace-c14-2-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:           trace2,
		KeyID:        k.ID,
		Trusted:      true,
		Model:        "model-c14",
		ClientFormat: "anthropic",
		Status:       "queued",
	})
	_ = s.InsertContractShape(trace2, "switcher", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"old_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())
	_ = s.InsertContractShape(trace2, "intact", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"new_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())

	consumer.ProcessTraceSync(trace2)

	// Judge should NOT have been called again for trace 2 because Diff produced no candidate for old_field
	if stub.Calls != 1 {
		t.Fatalf("expected stub judge calls to remain 1 (no candidates from trace 2), got %d", stub.Calls)
	}
}

func TestC15FindingReopenAndCounters(t *testing.T) {
	s := newTestStore(t)
	stub := NewStubJudge(map[string]string{
		"lost_field": "lost",
	})
	jm := NewJudgeManager(s, stub)
	consumer := NewConsumer(s, jm, nil)

	k, err := s.CreateAPIKey("k-c15")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetAPIKeyTrusted(k.ID, true)

	// Step 1: Trace 1 creates the finding and approves signature as "lost"
	trace1 := "trace-c15-1-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              trace1,
		KeyID:           k.ID,
		Trusted:         true,
		Model:           "model-c15",
		ClientFormat:    "anthropic",
		SwitcherVersion: "1.0.0+20260924T100000Z",
		Status:          "queued",
	})
	_ = s.InsertContractShape(trace1, "switcher", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"lost_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())
	_ = s.InsertContractShape(trace1, "intact", "request", `{"reducerVersion":1,"records":[]}`, time.Now().UnixMilli())
	consumer.ProcessTraceSync(trace1)

	finding, err := s.GetContractFindingByPath("model-c15", "anthropic", "request", "lost_field")
	if err != nil {
		t.Fatalf("finding not created: %v", err)
	}
	if finding.Count != 1 || finding.LastTrace != trace1 {
		t.Fatalf("expected count 1 and last_trace %s, got %d and %s", trace1, finding.Count, finding.LastTrace)
	}

	// Step 2: Trace 2 with open finding -> count and last_trace increase
	trace2 := "trace-c15-2-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              trace2,
		KeyID:           k.ID,
		Trusted:         true,
		Model:           "model-c15",
		ClientFormat:    "anthropic",
		SwitcherVersion: "1.0.0+20260924T100000Z",
		Status:          "queued",
	})
	_ = s.InsertContractShape(trace2, "switcher", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"lost_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())
	_ = s.InsertContractShape(trace2, "intact", "request", `{"reducerVersion":1,"records":[]}`, time.Now().UnixMilli())
	consumer.ProcessTraceSync(trace2)

	finding, _ = s.GetContractFinding(finding.ID)
	if finding.Count != 2 || finding.LastTrace != trace2 {
		t.Fatalf("expected count 2 and last_trace %s, got %d and %s", trace2, finding.Count, finding.LastTrace)
	}

	// Step 3: Resolve finding as 'fixed' (fixedIn should be 1.0.0+...)
	if err := ResolveFinding(s, finding.ID, "fixed", "key-admin", "resolved as fixed"); err != nil {
		t.Fatalf("resolve fixed: %v", err)
	}
	finding, _ = s.GetContractFinding(finding.ID)
	if finding.Status != "fixed" || finding.FixedIn != "1.0.0+20260924T100000Z" {
		t.Fatalf("expected fixed with fixedIn 1.0.0+, got status %s, fixedIn %s", finding.Status, finding.FixedIn)
	}

	// Step 4: Trace with 0.9.0 stays fixed
	trace09 := "trace-c15-09-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              trace09,
		KeyID:           k.ID,
		Trusted:         true,
		Model:           "model-c15",
		ClientFormat:    "anthropic",
		SwitcherVersion: "0.9.0+20260924T090000Z",
		Status:          "queued",
	})
	_ = s.InsertContractShape(trace09, "switcher", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"lost_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())
	_ = s.InsertContractShape(trace09, "intact", "request", `{"reducerVersion":1,"records":[]}`, time.Now().UnixMilli())
	consumer.ProcessTraceSync(trace09)

	finding, _ = s.GetContractFinding(finding.ID)
	if finding.Status != "fixed" {
		t.Fatalf("expected finding to stay fixed on 0.9.0, got %s", finding.Status)
	}

	// Step 5: Trace with 1.1.0 reopens fixed finding
	trace11 := "trace-c15-11-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              trace11,
		KeyID:           k.ID,
		Trusted:         true,
		Model:           "model-c15",
		ClientFormat:    "anthropic",
		SwitcherVersion: "1.1.0+20260924T110000Z",
		Status:          "queued",
	})
	_ = s.InsertContractShape(trace11, "switcher", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"lost_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())
	_ = s.InsertContractShape(trace11, "intact", "request", `{"reducerVersion":1,"records":[]}`, time.Now().UnixMilli())
	consumer.ProcessTraceSync(trace11)

	finding, _ = s.GetContractFinding(finding.ID)
	if finding.Status != "open" {
		t.Fatalf("expected fixed finding to reopen on 1.1.0, got %s", finding.Status)
	}

	// Step 6: Resolve finding as 'wontfix' by trusted key k
	if err := ResolveFinding(s, finding.ID, "wontfix", k.ID, "resolved wontfix by key"); err != nil {
		t.Fatalf("resolve wontfix: %v", err)
	}
	finding, _ = s.GetContractFinding(finding.ID)
	if finding.Status != "wontfix" {
		t.Fatalf("expected wontfix status, got %s", finding.Status)
	}
	if finding.FixedIn == "" {
		t.Fatal("expected wontfix to store fixedIn version")
	}

	// Step 7: Trace with 1.2.0 reopens wontfix set by key
	trace12 := "trace-c15-12-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              trace12,
		KeyID:           k.ID,
		Trusted:         true,
		Model:           "model-c15",
		ClientFormat:    "anthropic",
		SwitcherVersion: "1.2.0+20260924T120000Z",
		Status:          "queued",
	})
	_ = s.InsertContractShape(trace12, "switcher", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"lost_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())
	_ = s.InsertContractShape(trace12, "intact", "request", `{"reducerVersion":1,"records":[]}`, time.Now().UnixMilli())
	consumer.ProcessTraceSync(trace12)

	finding, _ = s.GetContractFinding(finding.ID)
	if finding.Status != "open" {
		t.Fatalf("expected wontfix set by key to reopen on 1.2.0, got %s", finding.Status)
	}

	// Step 8: Resolve as 'wontfix' by session
	if err := ResolveFinding(s, finding.ID, "wontfix", "session", "resolved wontfix by session"); err != nil {
		t.Fatalf("resolve wontfix session: %v", err)
	}

	// Step 9: Trace with 1.3.0 does NOT reopen wontfix set by session
	trace13 := "trace-c15-13-123456"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:              trace13,
		KeyID:           k.ID,
		Trusted:         true,
		Model:           "model-c15",
		ClientFormat:    "anthropic",
		SwitcherVersion: "1.3.0+20260924T130000Z",
		Status:          "queued",
	})
	_ = s.InsertContractShape(trace13, "switcher", "request", `{"reducerVersion":1,"records":[{"leaves":[{"path":"lost_field","type":"string","len":5}]}]}`, time.Now().UnixMilli())
	_ = s.InsertContractShape(trace13, "intact", "request", `{"reducerVersion":1,"records":[]}`, time.Now().UnixMilli())
	consumer.ProcessTraceSync(trace13)

	finding, _ = s.GetContractFinding(finding.ID)
	if finding.Status != "wontfix" {
		t.Fatalf("expected session wontfix to stay wontfix on 1.3.0, got %s", finding.Status)
	}
}
