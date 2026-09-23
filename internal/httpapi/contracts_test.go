package httpapi

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/auth"
	"github.com/louisphamdev/intact/internal/contract"
	"github.com/louisphamdev/intact/internal/store"
)

func newContractTestServer(t *testing.T, baseOverride map[string]string) (*api, *store.Store, http.Handler) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "contract_test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	authCfg := auth.NewConfig("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "master-test-token", []byte("session-secret-key-32-bytes-long!"), 3600)
	a, handler := newServer(s, baseOverride, authCfg)
	return a, s, handler
}

func TestContractsTraceRoutes(t *testing.T) {
	a, s, handler := newContractTestServer(t, nil)

	// Create an API key
	key, err := s.CreateAPIKey("agent-key")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	traceID := "trace-route-test-12345678"
	cap, ok := a.contractMgr.ClaimTraced(traceID, key.ID, false, "test", "anthropic", "v1/messages")
	if !ok || cap == nil {
		t.Fatal("claim traced failed")
	}

	now := time.Now().UTC()
	goodHalf := contract.HalfPayload{
		ToolRequest:     `{"model":"test","input":"ping"}`,
		ToolResponse:    `{"output":"pong"}`,
		ToolVersion:     "1.0.0",
		SwitcherVersion: "1.0.0+" + now.Format("20060102T150405Z"),
		Converter: contract.ConverterFormat{
			InFormat:  "anthropic",
			OutFormat: "openai",
		},
	}
	halfBytes, _ := json.Marshal(goodHalf)

	// 1. Unauthorized caller gets 401
	req := httptest.NewRequest("POST", "/api/contracts/traces/"+traceID+"/half", bytes.NewReader(halfBytes))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", rec.Code)
	}

	// 2. Another untrusted key cannot see or post half to this trace -> 404
	otherKey, _ := s.CreateAPIKey("other-key")
	req = httptest.NewRequest("POST", "/api/contracts/traces/"+traceID+"/half", bytes.NewReader(halfBytes))
	req.Header.Set("Authorization", "Bearer "+otherKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-opener key, got %d", rec.Code)
	}

	// 3. Status route by non-opener -> 404
	req = httptest.NewRequest("GET", "/api/contracts/traces/"+traceID, nil)
	req.Header.Set("Authorization", "Bearer "+otherKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-opener status, got %d", rec.Code)
	}

	// 4. Status route by opener -> 200 with status: capturing
	req = httptest.NewRequest("GET", "/api/contracts/traces/"+traceID, nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for opener status, got %d", rec.Code)
	}
	var stResp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &stResp)
	if stResp["status"] != "capturing" {
		t.Fatalf("expected status 'capturing', got %q", stResp["status"])
	}

	// 5. Submit half while capturing -> 202 Accepted
	req = httptest.NewRequest("POST", "/api/contracts/traces/"+traceID+"/half", bytes.NewReader(halfBytes))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for capturing half upload, got %d: %s", rec.Code, rec.Body.String())
	}

	// 6. Duplicate submit half -> 409 Conflict
	req = httptest.NewRequest("POST", "/api/contracts/traces/"+traceID+"/half", bytes.NewReader(halfBytes))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate half, got %d", rec.Code)
	}

	// 7. Invalid toolVersion -> 400 Bad Request
	badToolVerHalf := goodHalf
	badToolVerHalf.ToolVersion = "invalid-version"
	badBytes, _ := json.Marshal(badToolVerHalf)
	req = httptest.NewRequest("POST", "/api/contracts/traces/some-other-trace/half", bytes.NewReader(badBytes))
	req.Header.Set("Authorization", "Bearer master-test-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad toolVersion, got %d", rec.Code)
	}

	// 8. Future switcherVersion (> 1 day) -> 400 Bad Request
	badSwitcherVerHalf := goodHalf
	badSwitcherVerHalf.SwitcherVersion = "1.0.0+" + now.Add(48*time.Hour).Format("20060102T150405Z")
	badBytes, _ = json.Marshal(badSwitcherVerHalf)
	req = httptest.NewRequest("POST", "/api/contracts/traces/some-other-trace/half", bytes.NewReader(badBytes))
	req.Header.Set("Authorization", "Bearer master-test-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for future switcherVersion, got %d", rec.Code)
	}

	// 9. Unknown converter format -> 400 Bad Request
	badFmtHalf := goodHalf
	badFmtHalf.Converter.InFormat = "unknown_format_xyz"
	badBytes, _ = json.Marshal(badFmtHalf)
	req = httptest.NewRequest("POST", "/api/contracts/traces/some-other-trace/half", bytes.NewReader(badBytes))
	req.Header.Set("Authorization", "Bearer master-test-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown format, got %d", rec.Code)
	}

	// 10. Gzip bomb -> 413 Payload Too Large
	var bombBuf bytes.Buffer
	zw := gzip.NewWriter(&bombBuf)
	_, _ = zw.Write(make([]byte, 5*1024*1024))
	_ = zw.Close()
	req = httptest.NewRequest("POST", "/api/contracts/traces/"+traceID+"/half", &bombBuf)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("Content-Encoding", "gzip")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for gzip bomb, got %d", rec.Code)
	}
}

func TestV1TraceHeaderCapture(t *testing.T) {
	// Set up an upstream mock server
	var receivedTraceHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTraceHeader = r.Header.Get("X-Intact-Trace")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}]}`))
	}))
	defer upstream.Close()

	_, s, handler := newContractTestServer(t, map[string]string{"groq": upstream.URL})

	// Register a connection
	_, err := s.CreateConnection("groq", "conn-test", "secret-upstream-token")
	if err != nil {
		t.Fatalf("save conn: %v", err)
	}

	traceID := "trace-v1-test-header-12345"
	reqBody := `{"model":"groq/llama","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer master-test-token")
	req.Header.Set("X-Intact-Trace", traceID)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Upstream must NEVER receive X-Intact-Trace header!
	if receivedTraceHeader != "" {
		t.Fatalf("X-Intact-Trace header leaked to upstream: %q", receivedTraceHeader)
	}

	// Trace should have been captured and sealed
	tr, err := s.GetContractTrace(traceID)
	if err != nil || tr.ID == "" {
		t.Fatalf("trace not found in store: %v", err)
	}
	if tr.Status != "open" {
		t.Fatalf("expected status 'open', got %q", tr.Status)
	}

	// Shapes should have been stored for intact
	shapes, err := s.ListContractShapesForTrace(traceID)
	if err != nil {
		t.Fatalf("list shapes: %v", err)
	}
	if len(shapes) < 2 {
		t.Fatalf("expected at least 2 shapes (req + resp), got %d", len(shapes))
	}
}

func TestContractReadRoutesAndMCP(t *testing.T) {
	a, s, handler := newContractTestServer(t, nil)

	// Create an untrusted key and a trusted key
	untrustedKey, _ := s.CreateAPIKey("untrusted-caller")
	trustedKey, _ := s.CreateAPIKey("trusted-caller")
	_ = s.SetAPIKeyTrusted(trustedKey.ID, true)

	// Pre-seed some findings
	findingID := "fnd_test_123456"
	now := time.Now().UnixMilli()
	_ = s.UpsertContractFinding(store.ContractFinding{
		ID:                findingID,
		Model:             "claude-3-5-sonnet-20241022",
		ClientFormat:      "anthropic",
		Direction:         "response",
		Path:              "usage.cache_creation_input_tokens",
		Class:             "lost",
		Status:            "open",
		FirstTrace:        "trace-fnd-1",
		FirstTraceTrusted: true,
		LastTrace:         "trace-fnd-1",
		ExemptTrace:       "trace-fnd-1",
		Count:             1,
		CreatedAt:         now,
		UpdatedAt:         now,
	})

	// Pre-seed a shape fixture for trace-fnd-1
	shapeJSON := `{"reducerVersion":1,"records":[{"leaves":[{"path":"usage.cache_creation_input_tokens","type":"string","len":42,"hash":"abcdef123456"}]}]}`
	_ = s.InsertContractShape("trace-fnd-1", "switcher", "response", shapeJSON, now)
	_ = s.InsertContractShape("trace-fnd-1", "intact", "response", shapeJSON, now)

	// Pre-seed learned contract
	_ = s.UpsertContractLearned(store.ContractLearned{
		Kind:           "model",
		Subject:        "claude-3-5-sonnet-20241022",
		Direction:      "response",
		Half:           "intact",
		Format:         "anthropic",
		Path:           "content[].text",
		Type:           "string",
		Seen:           5,
		FirstSeen:      now,
		LastSeen:       now,
		ReducerVersion: 1,
	})

	// 1. GET /api/contracts/policy - any key
	req := httptest.NewRequest("GET", "/api/contracts/policy", nil)
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy status: got %d, want 200", rec.Code)
	}
	var polResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &polResp); err != nil {
		t.Fatalf("unmarshal policy: %v", err)
	}
	if _, ok := polResp["default"]; !ok {
		t.Fatalf("policy missing 'default' field: %s", rec.Body.String())
	}

	// 2. GET /api/contracts - any key
	req = httptest.NewRequest("GET", "/api/contracts", nil)
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("contracts index status: got %d, want 200", rec.Code)
	}

	// 3. GET /api/contracts/models/{model} - any key, no hash, no len for strings
	req = httptest.NewRequest("GET", "/api/contracts/models/claude-3-5-sonnet-20241022", nil)
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get model contract status: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rawModelResp := rec.Body.String()
	if strings.Contains(rawModelResp, `"hash"`) {
		t.Fatalf("model contract leaked hash: %s", rawModelResp)
	}
	if strings.Contains(rawModelResp, `"len"`) {
		t.Fatalf("model contract leaked string len: %s", rawModelResp)
	}

	// 4. GET /api/contracts/findings
	// 4a. Untrusted key gets 404
	req = httptest.NewRequest("GET", "/api/contracts/findings?status=open", nil)
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("untrusted caller got %d for findings, want 404", rec.Code)
	}

	// 4b. Trusted key gets findings with first_trace_trusted
	req = httptest.NewRequest("GET", "/api/contracts/findings?status=open", nil)
	req.Header.Set("Authorization", "Bearer "+trustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted caller got %d for findings, want 200", rec.Code)
	}
	var findingsResp []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &findingsResp); err != nil {
		// Could be {findings: [...]} or [...]
		var wrap struct {
			Findings []map[string]any `json:"findings"`
		}
		if err2 := json.Unmarshal(rec.Body.Bytes(), &wrap); err2 != nil {
			t.Fatalf("unmarshal findings: %v", err)
		}
		findingsResp = wrap.Findings
	}
	if len(findingsResp) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findingsResp))
	}
	if tr, ok := findingsResp[0]["first_trace_trusted"].(bool); !ok || !tr {
		t.Fatalf("finding must have first_trace_trusted=true: %+v", findingsResp[0])
	}

	// 5. GET /api/contracts/fixtures/{traceId}
	// 5a. Untrusted key gets 404
	req = httptest.NewRequest("GET", "/api/contracts/fixtures/trace-fnd-1", nil)
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("untrusted caller got %d for fixture, want 404", rec.Code)
	}

	// 5b. Trusted key gets fixture with len and delta counts, without hash
	req = httptest.NewRequest("GET", "/api/contracts/fixtures/trace-fnd-1", nil)
	req.Header.Set("Authorization", "Bearer "+trustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted caller got %d for fixture, want 200", rec.Code)
	}
	rawFixture := rec.Body.String()
	if strings.Contains(rawFixture, `"hash"`) {
		t.Fatalf("fixture leaked hash: %s", rawFixture)
	}
	if !strings.Contains(rawFixture, `"len"`) {
		t.Fatalf("fixture missing len: %s", rawFixture)
	}

	// 6. POST /api/contracts/findings/{id}/resolve
	// 6a. Untrusted key gets 404
	resolvePayload := `{"status":"fixed","note":"fixed in converter"}`
	req = httptest.NewRequest("POST", "/api/contracts/findings/"+findingID+"/resolve", strings.NewReader(resolvePayload))
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("untrusted caller got %d for resolve, want 404", rec.Code)
	}

	// 6b. Trusted key resolves finding
	req = httptest.NewRequest("POST", "/api/contracts/findings/"+findingID+"/resolve", strings.NewReader(resolvePayload))
	req.Header.Set("Authorization", "Bearer "+trustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted caller got %d for resolve, want 200: %s", rec.Code, rec.Body.String())
	}
	f, _ := s.GetContractFinding(findingID)
	if f.Status != "fixed" {
		t.Fatalf("expected finding status 'fixed', got %q", f.Status)
	}

	// 6c. Second resolve on now-fixed finding returns 409 Conflict
	req = httptest.NewRequest("POST", "/api/contracts/findings/"+findingID+"/resolve", strings.NewReader(resolvePayload))
	req.Header.Set("Authorization", "Bearer "+trustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on concurrent/already-fixed resolve, got %d", rec.Code)
	}

	// 7. MCP Tools
	// 7a. list_contracts callable by any key
	mcpListReq := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_contracts","arguments":{}}}`
	req = httptest.NewRequest("POST", "/mcp", strings.NewReader(mcpListReq))
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mcp list_contracts status: got %d", rec.Code)
	}

	// 7b. get_contract: returns no hash and no len for strings
	mcpGetReq := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_contract","arguments":{"model":"claude-3-5-sonnet-20241022"}}}`
	req = httptest.NewRequest("POST", "/mcp", strings.NewReader(mcpGetReq))
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mcp get_contract status: got %d", rec.Code)
	}
	rawMcpGet := rec.Body.String()
	if strings.Contains(rawMcpGet, `"hash"`) {
		t.Fatalf("mcp get_contract leaked hash: %s", rawMcpGet)
	}
	if strings.Contains(rawMcpGet, `"len"`) {
		t.Fatalf("mcp get_contract leaked string len: %s", rawMcpGet)
	}

	// 7c. list_contract_findings by untrusted key -> isError: true
	mcpFindingsReq := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_contract_findings","arguments":{}}}`
	req = httptest.NewRequest("POST", "/mcp", strings.NewReader(mcpFindingsReq))
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mcp list_contract_findings status: got %d", rec.Code)
	}
	var rpcResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rpcResp)
	resObj, _ := rpcResp["result"].(map[string]any)
	if isErr, _ := resObj["isError"].(bool); !isErr {
		t.Fatalf("untrusted caller should get isError:true on list_contract_findings: %s", rec.Body.String())
	}

	// 7d. list_contract_findings by trusted key -> success
	req = httptest.NewRequest("POST", "/mcp", strings.NewReader(mcpFindingsReq))
	req.Header.Set("Authorization", "Bearer "+trustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mcp list_contract_findings trusted status: got %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rpcResp)
	resObj, _ = rpcResp["result"].(map[string]any)
	if isErr, _ := resObj["isError"].(bool); isErr {
		t.Fatalf("trusted caller should not get error: %s", rec.Body.String())
	}
	_ = a
}

func TestContractsUIRoutes(t *testing.T) {
	a, s, handler := newContractTestServer(t, nil)

	// Create a session cookie
	sessionToken := a.auth.IssueSession()

	// 1. Session cookie grants access to /api/contracts/findings
	req := httptest.NewRequest("GET", "/api/contracts/findings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session access to findings failed: got %d", rec.Code)
	}

	// 2. Pre-seed traces and dropped counter
	now := time.Now().UnixMilli()
	_ = s.InsertContractTrace(store.ContractTrace{
		ID:             "trace-ui-1",
		Model:          "claude-3-5-sonnet",
		Tool:           "claude",
		Status:         "done",
		Created:        now,
		Trusted:        true,
		ReducerVersion: 1,
	})
	_ = s.IncrementContractCounter("dropped_traces", 3)

	// GET /api/contracts/traces
	req = httptest.NewRequest("GET", "/api/contracts/traces", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get traces status: got %d", rec.Code)
	}
	var tracesResp struct {
		Traces  []map[string]any `json:"traces"`
		Dropped int64            `json:"dropped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tracesResp); err != nil {
		t.Fatalf("unmarshal traces response: %v", err)
	}
	if len(tracesResp.Traces) == 0 {
		t.Fatal("expected at least 1 trace in response")
	}
	if tracesResp.Dropped != 3 {
		t.Fatalf("expected dropped count 3, got %d", tracesResp.Dropped)
	}

	// 3. Pre-seed finding with history
	_ = s.UpsertContractFinding(store.ContractFinding{
		ID:                "fnd-hist-1",
		Model:             "claude-3-5-sonnet",
		ClientFormat:      "anthropic",
		Direction:         "response",
		Path:              "usage.output_tokens",
		ReducerVersion:    1,
		Tool:              "claude",
		Class:             "lost",
		Confidence:        1.0,
		FirstTrace:        "trace-ui-1",
		FirstTraceTrusted: true,
		LastTrace:         "trace-ui-1",
		ExemptTrace:       "trace-ui-1",
		Count:             1,
		Status:            "open",
		CreatedAt:         now,
		UpdatedAt:         now,
	})
	_, _ = s.DB.Exec(`INSERT INTO contract_finding_history (finding_id, at, who, old_status, new_status, note)
		VALUES (?, ?, ?, '', 'open', 'opened by test')`, "fnd-hist-1", now, "session")

	// GET /api/contracts/findings/fnd-hist-1/history
	req = httptest.NewRequest("GET", "/api/contracts/findings/fnd-hist-1/history", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get finding history status: got %d", rec.Code)
	}
	var histResp []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &histResp); err != nil {
		t.Fatalf("unmarshal history response: %v", err)
	}
	if len(histResp) != 1 || histResp[0]["new_status"] != "open" {
		t.Fatalf("unexpected history resp: %+v", histResp)
	}

	// 4. Pre-seed proposed signature
	_ = s.InsertContractSignature(store.ContractSignature{
		Model:          "claude-3-5-sonnet",
		ClientFormat:   "anthropic",
		Direction:      "response",
		Path:           "thinking",
		Type:           "string",
		Verdict:        "noise",
		VerdictState:   "proposed",
		FirstTraceID:   "trace-ui-1",
		ReducerVersion: 1,
	})

	// GET /api/contracts/signatures?state=proposed
	req = httptest.NewRequest("GET", "/api/contracts/signatures?state=proposed", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get signatures status: got %d", rec.Code)
	}
	var sigResp []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sigResp); err != nil {
		t.Fatalf("unmarshal signatures resp: %v", err)
	}
	if len(sigResp) == 0 || sigResp[0]["verdict_state"] != "proposed" {
		t.Fatalf("unexpected signatures response: %+v", sigResp)
	}

	// POST /api/contracts/signatures/verdict (approve)
	apprBody := `{"model":"claude-3-5-sonnet","clientFormat":"anthropic","direction":"response","path":"thinking","type":"string","action":"approve"}`
	req = httptest.NewRequest("POST", "/api/contracts/signatures/verdict", strings.NewReader(apprBody))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve signature status: got %d, body: %s", rec.Code, rec.Body.String())
	}
	sig, _ := s.GetContractSignature("claude-3-5-sonnet", "anthropic", "response", "thinking", "string")
	if sig.VerdictState != "approved" {
		t.Fatalf("expected approved signature, got %s", sig.VerdictState)
	}
}

func TestC6TracedRequestTranslationErrorAbortsCapture(t *testing.T) {
	a, s, handler := newContractTestServer(t, map[string]string{"groq": "http://127.0.0.1:9999"})
	_, err := s.CreateConnection("groq", "acc-1", "key-1")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	key, err := s.CreateAPIKey("untrusted-key")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	_, tracedBefore, untrustedBefore := a.contractMgr.Slots(key.ID)
	traceID := "trace-c6-trans-err-123456"

	// Malformed JSON that passes bodyModel but fails decode in toProvider
	body := `{"model":"groq/llama", "bad": `
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("X-Intact-Trace", traceID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for translation error, got %d: %s", rec.Code, rec.Body.String())
	}

	_, tracedAfter, untrustedAfter := a.contractMgr.Slots(key.ID)
	if tracedAfter != tracedBefore || untrustedAfter != untrustedBefore {
		t.Fatalf("slot leak: before (traced=%d, untrusted=%d), after (traced=%d, untrusted=%d)",
			tracedBefore, untrustedBefore, tracedAfter, untrustedAfter)
	}

	tr, err := s.GetContractTrace(traceID)
	if err != nil {
		t.Fatalf("get trace: %v", err)
	}
	if tr.Status != "aborted" {
		t.Fatalf("expected trace status aborted, got %s", tr.Status)
	}
}

func TestC3OpenerAccessControl(t *testing.T) {
	a, s, handler := newContractTestServer(t, nil)

	untrustedKey, _ := s.CreateAPIKey("untrusted-key")
	keyA, _ := s.CreateAPIKey("key-a")
	keyBTrusted, _ := s.CreateAPIKey("key-b-trusted")
	_ = s.SetAPIKeyTrusted(keyBTrusted.ID, true)

	now := time.Now().UTC()
	goodHalf := contract.HalfPayload{
		ToolRequest:     `{"model":"test","input":"ping"}`,
		ToolResponse:    `{"output":"pong"}`,
		ToolVersion:     "1.0.0",
		SwitcherVersion: "1.0.0+" + now.Format("20060102T150405Z"),
		Converter: contract.ConverterFormat{
			InFormat:  "anthropic",
			OutFormat: "openai",
		},
	}
	halfBytes, _ := json.Marshal(goodHalf)

	// 1. Master-token trace: key_id == ""
	masterTraceID := "trace-master-12345678"
	capM, okM := a.contractMgr.ClaimTraced(masterTraceID, "", true, "test", "anthropic", "v1/messages")
	if !okM || capM == nil {
		t.Fatal("claim master trace failed")
	}

	// Untrusted key half -> 404, no switcher shape row
	req := httptest.NewRequest("POST", "/api/contracts/traces/"+masterTraceID+"/half", bytes.NewReader(halfBytes))
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for untrusted key posting to master-token trace, got %d", rec.Code)
	}

	shapes, _ := s.ListContractShapesForTrace(masterTraceID)
	for _, sh := range shapes {
		if sh.Half == "switcher" {
			t.Fatal("switcher shape row should not exist")
		}
	}

	// Untrusted key GET status -> 404
	req = httptest.NewRequest("GET", "/api/contracts/traces/"+masterTraceID, nil)
	req.Header.Set("Authorization", "Bearer "+untrustedKey.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for untrusted key getting master trace status, got %d", rec.Code)
	}

	// 2. Key-A trace: half from Key B (trusted or not) -> 404; from A -> accepted
	traceA := "trace-keya-12345678"
	capA, okA := a.contractMgr.ClaimTraced(traceA, keyA.ID, false, "test", "anthropic", "v1/messages")
	if !okA || capA == nil {
		t.Fatal("claim keyA trace failed")
	}

	// Half from Key B (trusted) -> 404
	req = httptest.NewRequest("POST", "/api/contracts/traces/"+traceA+"/half", bytes.NewReader(halfBytes))
	req.Header.Set("Authorization", "Bearer "+keyBTrusted.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for trusted key B posting to key A trace, got %d", rec.Code)
	}

	// Half from Key A -> accepted (202)
	req = httptest.NewRequest("POST", "/api/contracts/traces/"+traceA+"/half", bytes.NewReader(halfBytes))
	req.Header.Set("Authorization", "Bearer "+keyA.Key)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for key A posting to its own trace, got %d", rec.Code)
	}
}

func TestC2ReviewSignatureVerdictAdminOnly(t *testing.T) {
	a, s, handler := newContractTestServer(t, nil)
	sessionToken := a.auth.IssueSession()

	sig := store.ContractSignature{
		Model:          "claude-3-5-sonnet",
		ClientFormat:   "anthropic",
		Direction:      "response",
		Path:           "thinking",
		Type:           "string",
		Verdict:        "noise",
		VerdictState:   "proposed",
		FirstTraceID:   "trace-c2-1",
		ReducerVersion: 1,
	}
	if err := s.InsertContractSignature(sig); err != nil {
		t.Fatalf("insert signature: %v", err)
	}

	trustedKey, err := s.CreateAPIKey("trusted-worker")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := s.SetAPIKeyTrusted(trustedKey.ID, true); err != nil {
		t.Fatalf("set key trusted: %v", err)
	}

	apprBody := `{"model":"claude-3-5-sonnet","clientFormat":"anthropic","direction":"response","path":"thinking","type":"string","action":"approve"}`

	// 1. Trusted API key -> 403 Forbidden, signature stays proposed
	req := httptest.NewRequest("POST", "/api/contracts/signatures/verdict", strings.NewReader(apprBody))
	req.Header.Set("Authorization", "Bearer "+trustedKey.Key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for trusted API key, got %d", rec.Code)
	}
	gotSig, err := s.GetContractSignature("claude-3-5-sonnet", "anthropic", "response", "thinking", "string")
	if err != nil {
		t.Fatalf("get signature: %v", err)
	}
	if gotSig.VerdictState != "proposed" {
		t.Fatalf("expected signature to remain proposed, got %s", gotSig.VerdictState)
	}

	// 2. Master token -> 200 OK, signature approved
	req = httptest.NewRequest("POST", "/api/contracts/signatures/verdict", strings.NewReader(apprBody))
	req.Header.Set("Authorization", "Bearer master-test-token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for master token, got %d", rec.Code)
	}
	gotSig, err = s.GetContractSignature("claude-3-5-sonnet", "anthropic", "response", "thinking", "string")
	if err != nil {
		t.Fatalf("get signature: %v", err)
	}
	if gotSig.VerdictState != "approved" {
		t.Fatalf("expected signature to be approved, got %s", gotSig.VerdictState)
	}

	// Reset to proposed
	sig.VerdictState = "proposed"
	if err := s.InsertContractSignature(sig); err != nil {
		t.Fatalf("reset signature: %v", err)
	}

	// 3. Session cookie -> 200 OK, signature approved
	req = httptest.NewRequest("POST", "/api/contracts/signatures/verdict", strings.NewReader(apprBody))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for session cookie, got %d", rec.Code)
	}
	gotSig, err = s.GetContractSignature("claude-3-5-sonnet", "anthropic", "response", "thinking", "string")
	if err != nil {
		t.Fatalf("get signature: %v", err)
	}
	if gotSig.VerdictState != "approved" {
		t.Fatalf("expected signature to be approved, got %s", gotSig.VerdictState)
	}
}

func TestC18NoRawStorage(t *testing.T) {
	promptMarker := "PROMPT_SECRET_C18_MARKER_987654321_ABCD"
	answerMarker := "ANSWER_SECRET_C18_MARKER_123456789_WXYZ"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-c18","choices":[{"message":{"role":"assistant","content":"` + answerMarker + `"}}]}`))
	}))
	defer upstream.Close()

	a, s, handler := newContractTestServer(t, map[string]string{"groq": upstream.URL})
	_, err := s.CreateConnection("groq", "conn-c18", "secret-key")
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	traceID := "trace-c18-raw-storage-check-001"
	reqBody := `{"model":"groq/llama","messages":[{"role":"user","content":"` + promptMarker + `"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer master-test-token")
	req.Header.Set("X-Intact-Trace", traceID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("v1 request failed with status %d: %s", rec.Code, rec.Body.String())
	}

	// Upload switcher half
	now := time.Now().UTC()
	goodHalf := contract.HalfPayload{
		ToolRequest:     `{"model":"groq/llama","messages":[{"role":"user","content":"` + promptMarker + `"}]}`,
		ToolResponse:    `{"choices":[{"message":{"role":"assistant","content":"` + answerMarker + `"}` + `}]}`,
		ToolVersion:     "1.0.0",
		SwitcherVersion: "1.0.0+" + now.Format("20060102T150405Z"),
		Converter: contract.ConverterFormat{
			InFormat:  "anthropic",
			OutFormat: "openai",
		},
	}
	halfBytes, _ := json.Marshal(goodHalf)
	halfReq := httptest.NewRequest("POST", "/api/contracts/traces/"+traceID+"/half", bytes.NewReader(halfBytes))
	halfReq.Header.Set("Authorization", "Bearer master-test-token")
	halfRec := httptest.NewRecorder()
	handler.ServeHTTP(halfRec, halfReq)
	if halfRec.Code != http.StatusOK {
		t.Fatalf("half upload failed with status %d: %s", halfRec.Code, halfRec.Body.String())
	}

	// Run consumer
	a.consumer.ProcessTraceSync(traceID)

	// Table exemptions with one-line reason:
	exemptTables := map[string]string{
		"upstream_errors": "Stores raw error response bodies for upstream provider error diagnostics (pre-existing feature)",
	}

	// Scan every text column of every table in sqlite_master -> 0 hits
	tRows, err := s.DB.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer tRows.Close()

	var tables []string
	for tRows.Next() {
		var tbl string
		if err := tRows.Scan(&tbl); err == nil {
			tables = append(tables, tbl)
		}
	}
	tRows.Close()

	for _, tbl := range tables {
		if reason, ok := exemptTables[tbl]; ok {
			t.Logf("skipping exempt table %q: %s", tbl, reason)
			continue
		}
		// Query table columns
		cRows, err := s.DB.Query(`PRAGMA table_info(` + tbl + `)`)
		if err != nil {
			t.Fatalf("pragma table_info on %s: %v", tbl, err)
		}
		var cols []string
		for cRows.Next() {
			var cid int
			var name, colType string
			var notNull, pk int
			var dflt sql.NullString
			if err := cRows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err == nil {
				cols = append(cols, name)
			}
		}
		cRows.Close()

		for _, col := range cols {
			var promptHits, answerHits int
			_ = s.DB.QueryRow(`SELECT count(*) FROM `+tbl+` WHERE `+col+` LIKE ?`, "%"+promptMarker+"%").Scan(&promptHits)
			if promptHits > 0 {
				t.Fatalf("found %d prompt marker hits in table %q column %q", promptHits, tbl, col)
			}
			_ = s.DB.QueryRow(`SELECT count(*) FROM `+tbl+` WHERE `+col+` LIKE ?`, "%"+answerMarker+"%").Scan(&answerHits)
			if answerHits > 0 {
				t.Fatalf("found %d answer marker hits in table %q column %q", answerHits, tbl, col)
			}
		}
	}
}
