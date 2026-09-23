package contract

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReusedTraceIDNotCaptured(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)

	traceID := "trace-1234567890abcdef"
	c1, ok := m.ClaimTraced(traceID, "key-1", false, "model-1", "anthropic", "v1/messages")
	if !ok || c1 == nil {
		t.Fatal("first claim should succeed")
	}

	// Second claim with the same trace ID must be refused (proxied as usual, not captured).
	c2, ok := m.ClaimTraced(traceID, "key-1", false, "model-1", "anthropic", "v1/messages")
	if ok || c2 != nil {
		t.Fatal("reused trace id must not be captured")
	}
}

func TestCaptureUntrustedCaps(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)

	// Untrusted key can hold at most 2 open captures.
	c1, ok1 := m.ClaimTraced("trace-untrusted-111111", "untrusted-key", false, "m", "anthropic", "v1/messages")
	c2, ok2 := m.ClaimTraced("trace-untrusted-222222", "untrusted-key", false, "m", "anthropic", "v1/messages")
	if !ok1 || !ok2 || c1 == nil || c2 == nil {
		t.Fatal("first two untrusted claims should succeed")
	}

	// 3rd untrusted claim for same key must fail.
	c3, ok3 := m.ClaimTraced("trace-untrusted-333333", "untrusted-key", false, "m", "anthropic", "v1/messages")
	if ok3 || c3 != nil {
		t.Fatal("untrusted key exceeded cap of 2 open captures")
	}

	// A trusted key must still be able to claim a capture!
	cTrusted, okT := m.ClaimTraced("trace-trusted-4444444", "trusted-key", true, "m", "anthropic", "v1/messages")
	if !okT || cTrusted == nil {
		t.Fatal("trusted key should be able to claim even when untrusted key is at cap")
	}
}

func TestC5UntrustedCapsDoNotBlockTrusted(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)

	// Open slots for 8 untrusted keys (2 each attempted)
	for i := 0; i < 8; i++ {
		keyID := fmt.Sprintf("untrusted-key-%d", i)
		for j := 0; j < 2; j++ {
			traceID := fmt.Sprintf("trace-c5-untrusted-%d-%d-1234", i, j)
			c, ok := m.ClaimTraced(traceID, keyID, false, "model-1", "anthropic", "v1/messages")
			if !ok || c == nil {
				t.Fatalf("failed to claim untrusted slot key %d slot %d", i, j)
			}
		}
	}

	// ClaimTraced with trusted=true must still be ok
	traceTrusted := "trace-c5-trusted-12345678"
	cTrusted, ok := m.ClaimTraced(traceTrusted, "trusted-key", true, "model-1", "anthropic", "v1/messages")
	if !ok || cTrusted == nil {
		t.Fatal("ClaimTraced with trusted=true failed when 16 untrusted slots open")
	}
}

func TestGzipBombHalfGets413(t *testing.T) {
	// Create a gzip bomb: 5 MiB of zeroes compressed into few bytes
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zeros := make([]byte, 5*1024*1024)
	zw.Write(zeros)
	zw.Close()

	// Decompress and check bound
	_, err := DecompressHalf(buf.Bytes(), true)
	if err != ErrPayloadTooLarge {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
}

func TestGoldenSamplingLimits(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)

	// Untrusted key must NEVER be sampled for golden capture.
	_, ok := m.ClaimGolden("untrusted-key", false, "claude", "claude-cli/1.2.3 (Darwin)", "v1/messages")
	if ok {
		t.Fatal("untrusted caller must not be sampled for golden capture")
	}

	// Bad User-Agent or invalid version
	_, ok = m.ClaimGolden("trusted-key", true, "claude", "curl/7.68.0", "v1/messages")
	if ok {
		t.Fatal("bad user agent must not be sampled")
	}
	_, ok = m.ClaimGolden("trusted-key", true, "claude", "claude-cli/invalid_ver", "v1/messages")
	if ok {
		t.Fatal("invalid tool version must not be sampled")
	}

	// At most 3 samples per provider, endpoint and tool version per day.
	for i := 0; i < 3; i++ {
		c, ok := m.ClaimGolden("trusted-key", true, "claude", "claude-cli/1.0.0 darwin", "v1/messages")
		if !ok || c == nil {
			t.Fatalf("sample %d should succeed", i)
		}
		close(c.ProducerDone)
		_ = c.Seal(false, nil, nil)
	}
	// 4th sample of version 1.0.0 must fail.
	_, ok = m.ClaimGolden("trusted-key", true, "claude", "claude-cli/1.0.0 darwin", "v1/messages")
	if ok {
		t.Fatal("4th sample of same version should be rejected")
	}

	// At most 5 distinct versions per tool per day.
	// We already used 1.0.0 (version 1). Now use 4 more versions.
	for v := 1; v <= 4; v++ {
		ver := string(rune('1'+v)) + ".0.0"
		c, ok := m.ClaimGolden("trusted-key", true, "claude", "claude-cli/"+ver+" darwin", "v1/messages")
		if !ok || c == nil {
			t.Fatalf("version %s should succeed", ver)
		}
		close(c.ProducerDone)
		_ = c.Seal(false, nil, nil)
	}
	// 6th distinct version must fail.
	_, ok = m.ClaimGolden("trusted-key", true, "claude", "claude-cli/9.9.9 darwin", "v1/messages")
	if ok {
		t.Fatal("6th distinct tool version must be rejected")
	}
}

func TestValidateSwitcherVersion(t *testing.T) {
	// Must match ^\d{1,5}\.\d{1,5}\.\d{1,5}\+\d{8}T\d{6}Z$ and be valid UTC time not > server time + 1 day
	now := time.Now().UTC()
	goodVer := "1.2.3+" + now.Format("20060102T150405Z")
	if err := ValidateSwitcherVersion(goodVer, now); err != nil {
		t.Fatalf("good version rejected: %v", err)
	}

	// Two days in future
	future := now.Add(48 * time.Hour)
	futureVer := "1.2.3+" + future.Format("20060102T150405Z")
	if err := ValidateSwitcherVersion(futureVer, now); err == nil {
		t.Fatal("future switcherVersion > 1 day should be rejected")
	}

	// Invalid date time
	invalidDate := "1.2.3+99999999T999999Z"
	if err := ValidateSwitcherVersion(invalidDate, now); err == nil {
		t.Fatal("invalid time part should be rejected")
	}
}

func TestTraceLifecycleAndHalfCodes(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)
	var enqueued []string
	m.SetEnqueue(func(id string) {
		enqueued = append(enqueued, id)
	})

	traceID := "trace-lifecycle-test-12345"
	cap, ok := m.ClaimTraced(traceID, "key-1", true, "model-test", "anthropic", "v1/messages")
	if !ok || cap == nil {
		t.Fatal("claim traced failed")
	}
	cap.WriteReq([]byte(`{"model":"model-test","messages":[{"role":"user","content":"hello"}]}`))

	half := &HalfPayload{
		ToolRequest:     `{"model":"model-test","input":"hello"}`,
		ToolResponse:    `{"output":"world"}`,
		ToolVersion:     "1.0.0",
		SwitcherVersion: "1.0.0+20260923T120000Z",
		Converter: ConverterFormat{
			InFormat:  "anthropic",
			OutFormat: "openai",
		},
	}

	// 1. In capturing: SubmitHalf -> 202 Accepted (held)
	code, err := m.SubmitHalf(traceID, "key-1", true, half)
	if err != nil || code != 202 {
		t.Fatalf("expected 202 for capturing half upload, got %d, %v", code, err)
	}

	// 2. Already held in capturing: SubmitHalf again -> 409 Conflict
	code, err = m.SubmitHalf(traceID, "key-1", true, half)
	if code != 409 {
		t.Fatalf("expected 409 for duplicate half upload while capturing, got %d, %v", code, err)
	}

	// 3. Seal the capture: joins held half and transitions through open -> reducing -> queued
	cap.WriteResp([]byte(`{"content":[{"text":"world"}]}`))
	close(cap.ProducerDone)
	if err := cap.Seal(false, nil, nil); err != nil {
		t.Fatalf("seal failed: %v", err)
	}

	tr, err := s.GetContractTrace(traceID)
	if err != nil {
		t.Fatalf("get trace: %v", err)
	}
	if tr.Status != "queued" {
		t.Fatalf("expected status 'queued', got %q", tr.Status)
	}
	if len(enqueued) != 1 || enqueued[0] != traceID {
		t.Fatalf("expected trace %q to be enqueued, got %v", traceID, enqueued)
	}

	// 4. SubmitHalf again when already stored -> 409 Conflict
	code, err = m.SubmitHalf(traceID, "key-1", true, half)
	if code != 409 {
		t.Fatalf("expected 409 when already stored, got %d, %v", code, err)
	}

	// 5. SubmitHalf on non-open trace (e.g. lost) -> 410 Gone
	lostTraceID := "trace-lost-1234567890"
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:             lostTraceID,
		KeyID:          "key-1",
		Status:         "lost",
		Created:        time.Now().UnixMilli(),
		ReducerVersion: ReducerVersion,
	})
	code, err = m.SubmitHalf(lostTraceID, "key-1", true, half)
	if code != 410 {
		t.Fatalf("expected 410 for lost trace half upload, got %d, %v", code, err)
	}
}

func TestSealUnderRaceAndStreamAbort(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)

	traceID := "trace-abort-stream-test"
	cap, ok := m.ClaimTraced(traceID, "key-1", true, "model-test", "anthropic", "v1/messages")
	if !ok || cap == nil {
		t.Fatal("claim failed")
	}

	// Concurrently write to response buffer while sealing as aborted
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			cap.WriteResp([]byte("data: delta chunk\n\n"))
		}
		close(done)
	}()

	close(cap.ProducerDone)
	// Simulate stream disconnected in the middle: upstreamErr or clientErr
	err := cap.Seal(true, nil, errors.New("stream ended early"))
	if err != nil {
		t.Fatalf("seal err: %v", err)
	}
	<-done

	tr, err := s.GetContractTrace(traceID)
	if err != nil {
		t.Fatalf("get trace: %v", err)
	}
	if tr.Status != "aborted" {
		t.Fatalf("expected status 'aborted', got %q", tr.Status)
	}
}

func TestC6ReleaseOnAbort(t *testing.T) {
	s := newTestStore(t)
	m := NewCaptureManager(s, testKey)

	traceID := "trace-c6-abort-1234"
	c, ok := m.ClaimTraced(traceID, "key-1", false, "m", "anthropic", "v1/messages")
	if !ok || c == nil {
		t.Fatal("claim failed")
	}
	if m.tracedSlots != 1 || m.untrustedSlots["key-1"] != 1 {
		t.Fatalf("unexpected slots after claim: traced=%d untrusted=%d", m.tracedSlots, m.untrustedSlots["key-1"])
	}

	start := time.Now()
	c.ReleaseOnAbort()
	dur := time.Since(start)

	if dur >= 100*time.Millisecond {
		t.Fatalf("ReleaseOnAbort took %v, want < 100ms", dur)
	}
	if m.tracedSlots != 0 || m.untrustedSlots["key-1"] != 0 {
		t.Fatalf("slots not restored: traced=%d untrusted=%d", m.tracedSlots, m.untrustedSlots["key-1"])
	}
	tr, err := s.GetContractTrace(traceID)
	if err != nil {
		t.Fatalf("get trace: %v", err)
	}
	if tr.Status != "aborted" {
		t.Fatalf("expected status aborted, got %s", tr.Status)
	}
}

func TestC7SubmitHalfConcurrentWithSeal(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := newTestStore(t)
		m := NewCaptureManager(s, testKey)
		m.SetEnqueue(func(traceID string) {})

		traceID := fmt.Sprintf("trace-c7-%04d-12345678", i)
		cap, ok := m.ClaimTraced(traceID, "key-1", true, "test-model", "anthropic", "v1/messages")
		if !ok || cap == nil {
			t.Fatalf("iter %d: claim failed", i)
		}
		cap.WriteReq([]byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
		cap.WriteResp([]byte(`{"content":[{"type":"text","text":"hello"}]}`))
		close(cap.ProducerDone)

		half := &HalfPayload{
			ToolRequest:     `{"model":"test-model","input":"hi"}`,
			ToolResponse:    `{"output":"hello"}`,
			ToolVersion:     "1.0.0",
			SwitcherVersion: "1.0.0+20260924T000000Z",
			Converter: ConverterFormat{
				InFormat:  "anthropic",
				OutFormat: "anthropic",
			},
		}

		var submitCode int
		var submitErr error
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			_ = cap.Seal(false, nil, nil)
		}()

		go func() {
			defer wg.Done()
			submitCode, submitErr = m.SubmitHalf(traceID, "key-1", true, half)
		}()

		wg.Wait()

		if submitCode == http.StatusGone {
			t.Fatalf("iter %d: SubmitHalf returned 410", i)
		}
		if submitCode != http.StatusOK && submitCode != http.StatusAccepted {
			t.Fatalf("iter %d: unexpected SubmitHalf code %d: %v", i, submitCode, submitErr)
		}

		shapes, err := s.ListContractShapesForTrace(traceID)
		if err != nil {
			t.Fatalf("iter %d: list shapes: %v", i, err)
		}
		hasSwitcher := false
		for _, sh := range shapes {
			if sh.Half == "switcher" {
				hasSwitcher = true
				break
			}
		}
		if !hasSwitcher {
			t.Fatalf("iter %d: switcher shape not stored (submitCode=%d)", i, submitCode)
		}

		tr, err := s.GetContractTrace(traceID)
		if err != nil {
			t.Fatalf("iter %d: get trace: %v", i, err)
		}
		if tr.Status != "queued" && tr.Status != "done" && tr.Status != "reducing" {
			t.Fatalf("iter %d: trace status %s, want queued, done, or reducing", i, tr.Status)
		}
	}
}

func TestStartupReset(t *testing.T) {
	s := newTestStore(t)

	statuses := []string{"capturing", "open", "reducing", "queued", "done"}
	for _, st := range statuses {
		id := "trace-start-" + st
		_ = s.InsertContractTrace(store.ContractTrace{
			ID:             id,
			Status:         st,
			Created:        time.Now().UnixMilli(),
			ReducerVersion: ReducerVersion,
		})
	}

	if err := s.ResetStartupTraces(); err != nil {
		t.Fatalf("ResetStartupTraces: %v", err)
	}

	for _, st := range statuses {
		id := "trace-start-" + st
		tr, err := s.GetContractTrace(id)
		if err != nil {
			t.Fatalf("get trace %s: %v", id, err)
		}
		switch st {
		case "capturing", "open", "reducing":
			if tr.Status != "lost" {
				t.Fatalf("expected status 'lost' for initial status %s, got %s", st, tr.Status)
			}
		case "queued", "done":
			if tr.Status != st {
				t.Fatalf("expected status %s to be preserved, got %s", st, tr.Status)
			}
		}
	}
}
