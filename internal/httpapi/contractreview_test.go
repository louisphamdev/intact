package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/contract"
	"github.com/louisphamdev/intact/internal/store"
)

func addFinding(t *testing.T, s *store.Store, dir, path string) string {
	t.Helper()
	id := contract.NewFindingID()
	now := time.Now().UnixMilli()
	if err := s.UpsertContractFinding(store.ContractFinding{ID: id, Model: "antigravity/gemini-3.8-flash", ClientFormat: "anthropic",
		Direction: dir, Path: path, Class: "lost", FirstTrace: "t1", FirstTraceTrusted: true, LastTrace: "t1",
		Count: 3, Status: "open", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return id
}

func findingsByPath(t *testing.T, s *store.Store) map[string]store.ContractFinding {
	t.Helper()
	list, err := s.ListContractFindings("", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.ContractFinding{}
	for _, f := range list {
		out[f.Path] = f
	}
	return out
}

func TestContractReviewClosesConfidentExpectedLosses(t *testing.T) {
	up, states := fakeJev(map[string][2]any{
		"tools[].input_schema.$schema":                   {FindingDroppedByDesign, 0.9},
		"response.usageMetadata.cachedContentTokenCount": {FindingCarriedElsewhere, 0.8},
		"metadata.{*}#json.{*}":                          {FindingDataNoise, 0.7},
		"messages[].content[].tool_use_id":               {FindingRealLoss, 0.9},
		"system[].cache_control.ttl":                     {FindingDroppedByDesign, 0.4},
	})
	defer up.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("typesafe", "jev", "k")
	a, _ := newServer(s, map[string]string{"typesafe": up.URL}, nil)
	// A trusted switcher version, so a wontfix records fixedIn and a newer version could reopen it.
	if err := s.InsertContractTrace(store.ContractTrace{ID: "t1", Trusted: true, Model: "antigravity/gemini-3.8-flash",
		ClientFormat: "anthropic", Status: "done", SwitcherVersion: "1.0.0+20260101T000000Z"}); err != nil {
		t.Fatal(err)
	}
	addFinding(t, s, "request", "tools[].input_schema.$schema")
	addFinding(t, s, "response", "response.usageMetadata.cachedContentTokenCount")
	addFinding(t, s, "request", "metadata.{*}#json.{*}")
	lossID := addFinding(t, s, "request", "messages[].content[].tool_use_id")
	addFinding(t, s, "request", "system[].cache_control.ttl")

	n, err := a.reviewPending(context.Background())
	if err != nil || n != 5 {
		t.Fatalf("reviewed %d, %v", n, err)
	}
	got := findingsByPath(t, s)
	want := map[string]string{
		"tools[].input_schema.$schema":                   "wontfix " + FindingDroppedByDesign,
		"response.usageMetadata.cachedContentTokenCount": "wontfix " + FindingCarriedElsewhere,
		"metadata.{*}#json.{*}":                          "wontfix " + FindingDataNoise,
		"messages[].content[].tool_use_id":               "open " + FindingRealLoss,
		"system[].cache_control.ttl":                     "open " + FindingDroppedByDesign,
	}
	for p, w := range want {
		if g := got[p].Status + " " + got[p].ReviewCause; g != w {
			t.Errorf("%s: %q, want %q", p, g, w)
		}
	}
	if note := got["messages[].content[].tool_use_id"].ReviewNote; !strings.Contains(note, "no resolver is set") {
		t.Errorf("open note = %q", note)
	}
	hist, _ := s.GetLastFindingHistory(got["tools[].input_schema.$schema"].ID)
	if hist.Who != "review:typesafe/jev-latest" || hist.NewStatus != "wontfix" {
		t.Errorf("history = %+v", hist)
	}
	for _, st := range states() {
		f := st["facts"].(map[string]any)
		switch st["path"] {
		case "tools[].input_schema.$schema":
			if f["inside_tool_definition_schema"] != true || f["times_lost"].(float64) != 3 {
				t.Errorf("schema facts = %v", f)
			}
		case "metadata.{*}#json.{*}":
			if f["inside_user_data"] != true {
				t.Errorf("metadata facts = %v", f)
			}
		}
	}

	// A judged finding is not asked again.
	if n, _ := a.reviewPending(context.Background()); n != 0 {
		t.Errorf("judged again: %d", n)
	}
	// A newer switcher shows the same diff, so a finding the review closed stays closed.
	cand := contract.Candidate{Model: "antigravity/gemini-3.8-flash", ClientFormat: "anthropic", Direction: "request", Path: "tools[].input_schema.$schema"}
	newer := store.ContractTrace{ID: "t2", Trusted: true, SwitcherVersion: "99.0.0+20260101T000000Z"}
	if err := contract.HandleLostCandidate(s, newer, cand, ""); err != nil {
		t.Fatal(err)
	}
	if st := findingsByPath(t, s)["tools[].input_schema.$schema"].Status; st != "wontfix" {
		t.Errorf("review wontfix reopened: %s", st)
	}
	// A finding that reopens is judged again.
	if ok, _ := s.UpdateContractFindingStatus(lossID, "open", "expired", "system", "", ""); !ok {
		t.Fatal("expire failed")
	}
	cand.Path = "messages[].content[].tool_use_id"
	if err := contract.HandleLostCandidate(s, newer, cand, ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.reviewPending(context.Background()); n != 1 {
		t.Errorf("reopened finding judged %d times, want 1", n)
	}
}

func TestContractResolverSettlesTheUnsure(t *testing.T) {
	jev, _ := fakeJev(map[string][2]any{
		"system[].cache_control.ttl":       {FindingDroppedByDesign, 0.4},
		"messages[].content[].tool_use_id": {FindingRealLoss, 0.9},
		"messages[].content[].is_error":    {FindingCarriedElsewhere, 0.3},
	})
	defer jev.Close()
	actions := map[string]string{
		"system[].cache_control.ttl":       `{"cause":"dropped_by_design","action":"close","reason":"Gemini has no cache ttl"}`,
		"messages[].content[].tool_use_id": `{"cause":"real_loss","action":"keep_open","reason":"the tool result loses its call id"}`,
		// A close that names a real loss is not trusted.
		"messages[].content[].is_error": `{"cause":"real_loss","action":"close","reason":"whatever"}`,
	}
	var asked []string
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Messages []struct{ Content string }
		}
		json.NewDecoder(r.Body).Decode(&b)
		var st map[string]any
		json.Unmarshal([]byte(b.Messages[1].Content), &st)
		p := st["path"].(string)
		asked = append(asked, p)
		ans, _ := json.Marshal(actions[p])
		w.Write([]byte(`{"choices":[{"message":{"content":` + string(ans) + `}}]}`))
	}))
	defer chat.Close()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	s.CreateConnection("typesafe", "jev", "k")
	s.CreateConnection("groq", "g", "k")
	a, _ := newServer(s, map[string]string{"typesafe": jev.URL, "groq": chat.URL}, nil)
	for p := range actions {
		addFinding(t, s, "request", p)
	}
	// Without a resolver the unsure wait; set one, and it takes them over.
	if n, err := a.reviewPending(context.Background()); err != nil || n != 3 {
		t.Fatalf("first pass %d, %v", n, err)
	}
	if len(asked) != 0 {
		t.Fatalf("resolver asked before it was set: %v", asked)
	}
	resolver := "groq/llama-3.3-70b"
	if _, err := a.updateReviewConfig(nil, nil, &resolver, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := a.reviewPending(context.Background()); err != nil || n != 3 {
		t.Fatalf("back-review %d, %v", n, err)
	}
	got := findingsByPath(t, s)
	want := map[string]string{
		"system[].cache_control.ttl":       "wontfix",
		"messages[].content[].tool_use_id": "open",
		"messages[].content[].is_error":    "open",
	}
	for p, w := range want {
		f := got[p]
		if f.Status != w || f.ReviewBy != resolver {
			t.Errorf("%s: %s by %q, want %s by resolver", p, f.Status, f.ReviewBy, w)
		}
	}
	if note := got["messages[].content[].tool_use_id"].ReviewNote; !strings.Contains(note, "loses its call id") {
		t.Errorf("kept note = %q", note)
	}
	// Settled: nothing goes to either model again.
	if n, _ := a.reviewPending(context.Background()); n != 0 || len(asked) != 3 {
		t.Errorf("judged again: %d, resolver asked %v", n, asked)
	}
}
