package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// seedTwoChanges records two unacknowledged changes and returns their ids.
func seedTwoChanges(t *testing.T, s *store.Store) (int64, int64) {
	t.Helper()
	for _, p := range []string{"a_one", "a_two"} {
		if err := s.AddShapeChange(store.ShapeChange{Direction: "request", Provider: "claude",
			Endpoint: "v1/messages", Path: p, Kind: "added", NewType: "string"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	list, err := s.ListShapeChanges(store.ShapeChangeFilter{})
	if err != nil || len(list) != 2 {
		t.Fatalf("seed list = %v, %v", list, err)
	}
	return list[1].ID, list[0].ID
}

func TestDriftAckRefusesABodyItCannotRead(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	seedTwoChanges(t, s)
	h := New(s, nil)

	for _, tc := range []struct {
		name, body string
	}{
		{"trailing comma", `{"ids":[1,2,]}`},
		{"ids not a list", `{"ids":12}`},
		{"empty body", ``},
		{"over 64 KiB", `{"ids":[1],"pad":"` + strings.Repeat("x", 64<<10) + `"}`},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("POST", "/api/drift/ack", strings.NewReader(tc.body)))
		if rec.Code < 400 || rec.Code > 499 {
			t.Errorf("%s: status = %d, want 4xx (%s)", tc.name, rec.Code, rec.Body.String())
		}
		if n := s.CountUnackedShapeChanges(); n != 2 {
			t.Fatalf("%s: acked %d changes", tc.name, 2-n)
		}
	}
}

func TestDriftAckExplicitIDsAckOnlyThose(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	id1, _ := seedTwoChanges(t, s)
	h := New(s, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/api/drift/ack",
		strings.NewReader(`{"ids":[`+strconv.FormatInt(id1, 10)+`]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if n := s.CountUnackedShapeChanges(); n != 1 {
		t.Fatalf("unacked = %d, want 1", n)
	}
	list, _ := s.ListShapeChanges(store.ShapeChangeFilter{})
	for _, c := range list {
		if (c.ID == id1) != c.Acked {
			t.Errorf("change %d acked = %v", c.ID, c.Acked)
		}
	}
}

func TestDriftAckEmptyListStillAcksAll(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	seedTwoChanges(t, s)
	h := New(s, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/api/drift/ack", strings.NewReader(`{"ids":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if n := s.CountUnackedShapeChanges(); n != 0 {
		t.Fatalf("unacked = %d, want 0", n)
	}
}

func TestMCPAckDriftChangesRefusesNonIntegerIDs(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	seedTwoChanges(t, s)
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)

	for _, args := range []string{`{"ids":["12"]}`, `{"ids":[1,"2"]}`, `{"ids":[1.5]}`, `{"ids":"12"}`} {
		m := rpc(t, h, cfg.APIToken,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ack_drift_changes","arguments":`+args+`}}`)
		text, isErr := toolText(t, m)
		if !isErr {
			t.Errorf("ids %s: no tool error (%s)", args, text)
		}
		if n := s.CountUnackedShapeChanges(); n != 2 {
			t.Fatalf("ids %s: acked %d changes", args, 2-n)
		}
	}
	m := rpc(t, h, cfg.APIToken,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ack_drift_changes","arguments":{"ids":[]}}}`)
	if _, isErr := toolText(t, m); isErr {
		t.Fatal("an empty list must still acknowledge everything")
	}
	if n := s.CountUnackedShapeChanges(); n != 0 {
		t.Fatalf("unacked = %d, want 0", n)
	}
}

func TestDriftChangesMasksSampleForDashboardKey(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)

	if err := s.AddShapeChange(store.ShapeChange{
		Direction: "request",
		Provider:  "claude",
		Endpoint:  "v1/messages",
		Path:      "system_prompt",
		Kind:      "added",
		NewType:   "string",
		Sample:    "top secret prompt",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	key, err := s.CreateAPIKey("dashboard-caller")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// 1. REST with dashboard key -> sample is masked to ""
	req := loopbackRequest("GET", "/api/drift/changes", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("REST key status = %d: %s", rec.Code, rec.Body.String())
	}
	var restResp struct {
		Changes []store.ShapeChange `json:"changes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &restResp); err != nil {
		t.Fatalf("unmarshal REST: %v", err)
	}
	if len(restResp.Changes) == 0 {
		t.Fatal("REST: no changes returned")
	}
	for _, c := range restResp.Changes {
		if c.Sample != "" {
			t.Errorf("REST dashboard key saw sample %q, want empty", c.Sample)
		}
	}

	// 2. MCP list_drift_changes with dashboard key -> sample is masked to ""
	m := rpc(t, h, key.Key, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_drift_changes","arguments":{}}}`)
	text, isErr := toolText(t, m)
	if isErr {
		t.Fatalf("MCP tool error: %s", text)
	}
	var mcpChanges []store.ShapeChange
	if err := json.Unmarshal([]byte(text), &mcpChanges); err != nil {
		t.Fatalf("unmarshal MCP text: %v", err)
	}
	if len(mcpChanges) == 0 {
		t.Fatal("MCP: no changes returned")
	}
	for _, c := range mcpChanges {
		if c.Sample != "" {
			t.Errorf("MCP dashboard key saw sample %q, want empty", c.Sample)
		}
	}

	// 3. REST with master token -> sees sample
	reqMaster := loopbackRequest("GET", "/api/drift/changes", nil)
	reqMaster.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	recMaster := httptest.NewRecorder()
	h.ServeHTTP(recMaster, reqMaster)
	if recMaster.Code != http.StatusOK {
		t.Fatalf("REST master status = %d", recMaster.Code)
	}
	var restMasterResp struct {
		Changes []store.ShapeChange `json:"changes"`
	}
	if err := json.Unmarshal(recMaster.Body.Bytes(), &restMasterResp); err != nil {
		t.Fatalf("unmarshal REST master: %v", err)
	}
	if len(restMasterResp.Changes) == 0 || restMasterResp.Changes[0].Sample != "top secret prompt" {
		t.Errorf("REST master saw sample %q, want %q", restMasterResp.Changes[0].Sample, "top secret prompt")
	}

	// 4. MCP with master token -> sees sample
	mMaster := rpc(t, h, cfg.APIToken, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_drift_changes","arguments":{}}}`)
	textMaster, isErr := toolText(t, mMaster)
	if isErr {
		t.Fatalf("MCP master error: %s", textMaster)
	}
	var mcpMasterChanges []store.ShapeChange
	if err := json.Unmarshal([]byte(textMaster), &mcpMasterChanges); err != nil {
		t.Fatalf("unmarshal MCP master text: %v", err)
	}
	if len(mcpMasterChanges) == 0 || mcpMasterChanges[0].Sample != "top secret prompt" {
		t.Errorf("MCP master saw sample %q, want %q", mcpMasterChanges[0].Sample, "top secret prompt")
	}

	// 5. REST with session -> sees sample
	sessionToken := cfg.IssueSession()
	reqSession := loopbackRequest("GET", "/drift/changes", nil)
	reqSession.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	recSession := httptest.NewRecorder()
	h.ServeHTTP(recSession, reqSession)
	if recSession.Code != http.StatusOK {
		t.Fatalf("REST session status = %d: %s", recSession.Code, recSession.Body.String())
	}
	var restSessionResp struct {
		Changes []store.ShapeChange `json:"changes"`
	}
	if err := json.Unmarshal(recSession.Body.Bytes(), &restSessionResp); err != nil {
		t.Fatalf("unmarshal REST session: %v", err)
	}
	if len(restSessionResp.Changes) == 0 || restSessionResp.Changes[0].Sample != "top secret prompt" {
		t.Errorf("REST session saw sample %q, want %q", restSessionResp.Changes[0].Sample, "top secret prompt")
	}
}
