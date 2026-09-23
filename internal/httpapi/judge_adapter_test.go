package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/contract"
	"github.com/louisphamdev/intact/internal/store"
)

func TestC1JudgeIntegrationWithDecisionModel(t *testing.T) {
	// Part 1: No decision model configured -> ProcessCandidates takes no lease (lease_until 0) and does not change dayCount
	s1, _ := store.Open(filepath.Join(t.TempDir(), "s1.db"))
	defer s1.Close()

	a1, _ := newServer(s1, nil, nil)
	defer a1.consumer.Stop()

	// Candidate to test
	cand1 := contract.Candidate{
		Model:        "model-c1-no-dm",
		ClientFormat: "anthropic",
		Direction:    "request",
		Path:         "unconfigured_field",
		Type:         "string",
	}

	err := a1.consumer.GetJudgeManager().ProcessCandidates(t.Context(), "key-1", "trace-1", []contract.Candidate{cand1}, nil, nil, nil)
	if err != nil {
		t.Fatalf("processCandidates: %v", err)
	}

	sig1, err := s1.GetContractSignature(cand1.Model, cand1.ClientFormat, cand1.Direction, cand1.Path, cand1.Type)
	if err != nil {
		t.Fatalf("get signature: %v", err)
	}
	if sig1.LeaseUntil != 0 {
		t.Fatalf("expected lease_until 0 when no decision model configured, got %d", sig1.LeaseUntil)
	}
	if dayCount := a1.consumer.GetJudgeManager().GetDayCount("key-1:" + time.Now().UTC().Format("2006-01-02")); dayCount != 0 {
		t.Fatalf("expected dayCount 0 when no decision model configured, got %d", dayCount)
	}

	// Part 2: Decision model configured -> exactly one askJev request, candidate data only in untrusted_shape, lost answer opens one finding
	s2, _ := store.Open(filepath.Join(t.TempDir(), "s2.db"))
	defer s2.Close()

	var jevCalls atomic.Int32
	var capturedStates []map[string]any
	var mu sync.Mutex

	jevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jevCalls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var reqBody struct {
			Model     string         `json:"model"`
			State     string         `json:"state"`
			Questions map[string]any `json:"questions"`
		}
		_ = json.Unmarshal(raw, &reqBody)

		var st map[string]any
		_ = json.Unmarshal([]byte(reqBody.State), &st)
		// L4: a candidate path is untrusted; it may appear only inside untrusted_shape.
		if qs, _ := json.Marshal(reqBody.Questions); strings.Contains(string(qs), "missing_cand_path") {
			t.Errorf("a candidate path reached the questions: %s", qs)
		}

		mu.Lock()
		capturedStates = append(capturedStates, st)
		mu.Unlock()

		// Return 'lost' answer for candidate field
		resp := map[string]any{
			"model": "jev-latest",
			"answers": map[string]any{
				"c0": map[string]any{
					"type":   "choice",
					"choice": "lost",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer jevServer.Close()

	// Configure typesafe provider and decisionModel
	_, _ = s2.CreateConnection("typesafe", "jev", "dummy-key")
	_ = s2.SetSetting(reviewConfigKey, `{"enabled":true,"decisionModel":"typesafe/jev-latest"}`)

	a2, _ := newServer(s2, map[string]string{"typesafe": jevServer.URL}, nil)
	defer a2.consumer.Stop()

	k, err := s2.CreateAPIKey("k-c1")
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.SetAPIKeyTrusted(k.ID, true)

	// One trusted trace with one new candidate
	traceID := "trace-c1-trusted-123456"
	cap, ok := a2.contractMgr.ClaimTraced(traceID, k.ID, true, "model-c1", "anthropic", "v1/messages")
	if !ok || cap == nil {
		t.Fatal("claim failed")
	}
	cap.WriteReq([]byte(`{"model":"model-c1","messages":[{"role":"user","content":"hi"}]}`))
	cap.WriteResp([]byte(`{"content":[{"type":"text","text":"hello"}]}`))
	close(cap.ProducerDone)
	if err := cap.Seal(false, nil, nil); err != nil {
		t.Fatalf("seal failed: %v", err)
	}

	// Submit half with candidate "missing_cand_path" in toolRequest
	half := &contract.HalfPayload{
		ToolRequest:     `{"model":"model-c1","messages":[{"role":"user","content":"hi"}],"missing_cand_path":"val"}`,
		ToolResponse:    `{"content":[{"type":"text","text":"hello"}]}`,
		ToolVersion:     "1.0.0",
		SwitcherVersion: "1.0.0+20260924T000000Z",
		Converter: contract.ConverterFormat{
			InFormat:  "anthropic",
			OutFormat: "anthropic",
		},
	}
	code, err := a2.contractMgr.SubmitHalf(traceID, k.ID, true, half)
	if code != http.StatusOK {
		t.Fatalf("submit half code %d, err %v", code, err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tr, _ := s2.GetContractTrace(traceID)
		if tr.Status != "queued" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Check 1: exactly one askJev request sent
	if calls := jevCalls.Load(); calls != 1 {
		t.Fatalf("expected exactly 1 askJev request, got %d", calls)
	}

	// Check 2: candidate data only in untrusted_shape
	mu.Lock()
	if len(capturedStates) != 1 {
		t.Fatalf("expected 1 captured state, got %d", len(capturedStates))
	}
	st := capturedStates[0]
	mu.Unlock()

	untrustedShape, hasUntrusted := st["untrusted_shape"].(map[string]any)
	if !hasUntrusted {
		t.Fatalf("expected 'untrusted_shape' field in state: %v", st)
	}

	// Verify candidate data does NOT appear outside untrusted_shape
	stCopy := make(map[string]any)
	for k, v := range st {
		if k != "untrusted_shape" {
			stCopy[k] = v
		}
	}
	stOutsideBytes, _ := json.Marshal(stCopy)
	if strings.Contains(string(stOutsideBytes), "missing_cand_path") {
		t.Fatalf("candidate data appeared outside untrusted_shape: %s", string(stOutsideBytes))
	}

	// Verify candidate data DOES appear inside untrusted_shape
	stInsideBytes, _ := json.Marshal(untrustedShape)
	if !strings.Contains(string(stInsideBytes), "missing_cand_path") {
		t.Fatalf("candidate data missing from untrusted_shape: %s", string(stInsideBytes))
	}

	// Check 3: lost answer opens one finding
	findings, err := s2.ListContractFindings("open", 0, true)
	if err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 open finding, got %d", len(findings))
	}
	if findings[0].Path != "missing_cand_path" {
		t.Fatalf("expected finding for 'missing_cand_path', got '%s'", findings[0].Path)
	}
}
