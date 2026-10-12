package websearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostedRecognisesTheHostedToolAndNotAClientTool(t *testing.T) {
	for body, want := range map[string]bool{
		`{"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}]}`:                      true,
		`{"tools":[{"type":"web_search_20260101","name":"web_search"}]}`:                                   true,
		`{"tools":[{"name":"WebSearch"}]}`:                                                                 true,
		`{"tools":[{"name":"WebSearch","input_schema":{"type":"object"}}]}`:                                false,
		`{"tools":[{"name":"Read","input_schema":{}},{"type":"web_search_20250305","name":"web_search"}]}`: true,
		`{"tools":[{"name":"Read","input_schema":{}}]}`:                                                    false,
		`{"messages":[]}`: false,
		`not json`:        false,
	} {
		if got := Hosted([]byte(body)); got != want {
			t.Errorf("%s -> %v, want %v", body, got, want)
		}
	}
}

func TestQueryReadsClaudeCodesWordingAndAnyOtherMessage(t *testing.T) {
	for body, want := range map[string]string{
		`{"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: tin công nghệ 2026"}]}]}`: "tin công nghệ 2026",
		`{"messages":[{"role":"user","content":"PERFORM A WEB SEARCH FOR THE QUERY:   go 1.27  "}]}`:                                 "go 1.27",
		`{"messages":[{"role":"user","content":"who won the match"},{"role":"assistant","content":"x"}]}`:                            "who won the match",
		`{"messages":[]}`: "",
	} {
		if got := Query([]byte(body)); got != want {
			t.Errorf("%s -> %q, want %q", body, got, want)
		}
	}
	long := strings.Repeat("a", 900)
	if got := Query([]byte(`{"messages":[{"role":"user","content":"` + long + `"}]}`)); len([]rune(got)) != 400 {
		t.Errorf("a query of %d runes was not cut to 400", len([]rune(got)))
	}
}

func TestRewriteStripsTheHostedToolAndAddsTheResults(t *testing.T) {
	out, err := Rewrite([]byte(`{"model":"m","system":"be brief","tool_choice":{"type":"tool","name":"web_search"},
	 "tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"q"}]}`),
		"go 1.27", []Result{{Title: "Go", URL: "https://go.dev", Snippet: "release", Age: "2 days ago"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tools"] != nil || m["tool_choice"] != nil {
		t.Errorf("the hosted tool stayed: %s", out)
	}
	sys := m["system"].(string)
	for _, want := range []string{"be brief", `"go 1.27"`, "[1] Go", "URL: https://go.dev", "Age: 2 days ago", "release"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system misses %q: %s", want, sys)
		}
	}
}

func TestRewriteKeepsTheClientsOwnToolsAndBlocksInASystemList(t *testing.T) {
	out, _ := Rewrite([]byte(`{"system":[{"type":"text","text":"a"}],"tool_choice":{"type":"auto"},
	 "tools":[{"name":"Read","input_schema":{}},{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`), "q", nil, "")
	var m map[string]any
	json.Unmarshal(out, &m)
	if tools := m["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["name"] != "Read" {
		t.Errorf("tools = %v", m["tools"])
	}
	if m["tool_choice"] == nil {
		t.Error("a tool_choice that does not name the search was dropped")
	}
	sys := m["system"].([]any)
	if len(sys) != 2 || !strings.Contains(sys[1].(map[string]any)["text"].(string), "returned no results") {
		t.Errorf("system = %v", sys)
	}
	out, _ = Rewrite([]byte(`{"messages":[]}`), "q", nil, "the key was refused")
	if !strings.Contains(string(out), "could not be run (the key was refused)") {
		t.Errorf("failure not stated: %s", out)
	}
}

// fakeService stands in for one search service and keeps what it was asked.
func fakeService(t *testing.T, answer string) (*httptest.Server, *http.Request, *string) {
	t.Helper()
	var last http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last, body = *r.Clone(context.Background()), string(b)
		io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv, &last, &body
}

func TestEachServiceIsCalledWithItsKeyAndItsAnswerIsRead(t *testing.T) {
	for _, tc := range []struct {
		provider, answer, header, wantHeader string
		post                                 bool
	}{
		{"brave", `{"web":{"results":[{"title":"T","url":"https://u","description":"D","age":"1 day"}]}}`, "X-Subscription-Token", "k", false},
		{"tavily", `{"results":[{"title":"T","url":"https://u","content":"D"}]}`, "Authorization", "Bearer k", true},
		{"serper", `{"organic":[{"title":"T","link":"https://u","snippet":"D","date":"1 day"}]}`, "X-Api-Key", "k", true},
		{"exa", `{"results":[{"title":"T","url":"https://u","text":"D","publishedDate":"2026-01-01"}]}`, "X-Api-Key", "k", true},
	} {
		srv, last, body := fakeService(t, tc.answer)
		s, err := New(Config{Provider: tc.provider, Key: "k", BaseURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Search(context.Background(), "go 1.27", 3)
		if err != nil || len(res) != 1 || res[0].Title != "T" || res[0].URL != "https://u" || res[0].Snippet != "D" {
			t.Fatalf("%s: res = %+v, err = %v", tc.provider, res, err)
		}
		if got := last.Header.Get(tc.header); got != tc.wantHeader {
			t.Errorf("%s: %s = %q, want %q", tc.provider, tc.header, got, tc.wantHeader)
		}
		if tc.post != (last.Method == http.MethodPost) {
			t.Errorf("%s: method = %s", tc.provider, last.Method)
		}
		if got := last.URL.Query().Get("q") + *body; !strings.Contains(got, "go 1.27") {
			t.Errorf("%s: the query did not reach the service: %s %s", tc.provider, last.URL, *body)
		}
	}
}

func TestAServiceThatRefusesIsAnErrorWithItsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"bad key"}`)
	}))
	defer srv.Close()
	s, _ := New(Config{Provider: "brave", Key: "k", BaseURL: srv.URL})
	if _, err := s.Search(context.Background(), "q", 1); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestNewRefusesAnUnknownProviderAndAMissingKey(t *testing.T) {
	if _, err := New(Config{Provider: "nope", Key: "k"}); err == nil || !strings.Contains(err.Error(), "brave") {
		t.Errorf("err = %v", err)
	}
	if _, err := New(Config{Provider: "brave"}); err == nil || !strings.Contains(err.Error(), "key") {
		t.Errorf("err = %v", err)
	}
}

func TestFromEnvIsNilUntilAProviderIsSet(t *testing.T) {
	t.Setenv("INTACT_SEARCH_PROVIDER", "")
	if FromEnv() != nil {
		t.Error("web search is configured with nothing set")
	}
	t.Setenv("INTACT_SEARCH_PROVIDER", " Tavily ")
	t.Setenv("INTACT_SEARCH_KEY", " k ")
	t.Setenv("INTACT_SEARCH_COUNT", "7")
	if c := FromEnv(); c == nil || c.Provider != "tavily" || c.Key != "k" || c.Count != 7 {
		t.Errorf("config = %+v", c)
	}
}
