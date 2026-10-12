package servertools

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

func TestFillClientTools(t *testing.T) {
	body := `{"model":"m","tools":[
		{"type":"bash_20250124","name":"bash"},
		{"type":"text_editor_20250124","name":"str_replace_editor"},
		{"type":"text_editor_20250728","name":"str_replace_based_edit_tool","max_characters":100},
		{"type":"memory_20250818","name":"memory"},
		{"type":"web_search_20250305","name":"web_search"},
		{"type":"web_fetch_20250910","name":"web_fetch"},
		{"type":"tool_search_tool_regex_20251119","name":"tool_search_tool_regex"},
		{"type":"advisor_20260301","name":"advisor"},
		{"type":"computer_20250124","name":"computer"},
		{"name":"mine","description":"d","input_schema":{"type":"object"}}]}`
	out, dropped := FillClientTools([]byte(body))
	m, _ := decode(out)
	by := map[string]obj{}
	for _, tl := range list(m["tools"]) {
		by[str(asObj(tl)["name"])] = asObj(tl)
	}
	for _, n := range []string{"bash", "str_replace_editor", "str_replace_based_edit_tool", "memory"} {
		if by[n]["input_schema"] == nil || str(by[n]["description"]) == "" {
			t.Errorf("%s was not given a schema: %v", n, by[n])
		}
	}
	enum := func(tool string) []any {
		return list(asObj(asObj(asObj(by[tool]["input_schema"])["properties"])["command"])["enum"])
	}
	if len(enum("str_replace_editor")) != 5 || len(enum("str_replace_based_edit_tool")) != 4 {
		t.Errorf("undo_edit belongs to the 2025-01 editor only: %v / %v", enum("str_replace_editor"), enum("str_replace_based_edit_tool"))
	}
	if len(enum("memory")) != 6 {
		t.Errorf("memory commands: %v", enum("memory"))
	}
	if by["web_search"]["input_schema"] != nil || by["web_fetch"]["input_schema"] != nil {
		t.Error("a tool intact runs itself was given a client schema")
	}
	if strings.Join(dropped, ",") != "advisor_20260301 (advisor),computer_20250124 (computer)" {
		t.Errorf("dropped = %v", dropped)
	}
	if out2, d2 := FillClientTools([]byte(`{"tools":[{"name":"x","input_schema":{}}]}`)); string(out2) != `{"tools":[{"name":"x","input_schema":{}}]}` || len(d2) != 0 {
		t.Errorf("a request with nothing to fill changed: %s %v", out2, d2)
	}
}

func TestToolSearch(t *testing.T) {
	defs := []obj{
		{"name": "get_weather", "description": "Get the weather at a location", "input_schema": obj{"properties": obj{"location": obj{"description": "city name"}}}},
		{"name": "search_files", "description": "Search files in the workspace", "input_schema": obj{"properties": obj{"query": obj{"description": "text to look for"}}}},
		{"name": "send_email", "description": "Send an email to a person"},
	}
	var items []searchable
	for _, d := range defs {
		items = append(items, searchableOf(d))
	}
	hits, err := regexSearch(items, "WEATHER", 5)
	if err != nil || len(hits) != 1 || hits[0] != "get_weather" {
		t.Errorf("regex weather = %v, %v", hits, err)
	}
	hits, _ = regexSearch(items, "get_.*|send_", 5)
	if len(hits) != 2 {
		t.Errorf("regex alternation = %v", hits)
	}
	if hits, _ = regexSearch(items, "city", 5); len(hits) != 1 {
		t.Errorf("regex must search argument descriptions: %v", hits)
	}
	if hits, _ = regexSearch(items, ".", 1); len(hits) != 1 {
		t.Errorf("limit ignored: %v", hits)
	}
	for _, bad := range []string{"(unclosed", strings.Repeat("a", 201)} {
		_, err := regexSearch(items, bad, 5)
		if te, ok := err.(*toolSearchError); !ok || te.code != "invalid_tool_input" {
			t.Errorf("pattern %.20q: error = %v", bad, err)
		}
	}
	hits, _ = bm25Search(items, "I want to email someone", 5)
	if len(hits) == 0 || hits[0] != "send_email" {
		t.Errorf("bm25 email = %v", hits)
	}
	if hits, _ = bm25Search(items, "zzz nothing", 5); len(hits) != 0 {
		t.Errorf("bm25 no match = %v", hits)
	}
}

func TestHTMLToText(t *testing.T) {
	text, title := htmlToText(`<html><head><title> A &amp; B </title><style>p{}</style></head>
<body><script>var x=1;</script><h1>Head</h1><p>One &lt;two&gt;</p><!-- hidden --><p>Three<br>four</p></body></html>`)
	if title != "A & B" {
		t.Errorf("title = %q", title)
	}
	if text != "Head\nOne <two>\nThree\nfour" && text != "Head\n\nOne <two>\n\nThree\nfour" {
		t.Errorf("text = %q", text)
	}
	for _, leak := range []string{"var x", "p{}", "hidden", "<"} {
		if strings.Contains(strings.ReplaceAll(text, "<two>", ""), leak) {
			t.Errorf("text leaks %q: %q", leak, text)
		}
	}
}

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700::1111": true,
		"127.0.0.1": false, "::1": false, "10.0.0.5": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "224.0.0.1": false, "fe80::1": false, "fd00::1": false,
	} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestCheckURL(t *testing.T) {
	cfg := FetchConfig{Allowed: []string{"example.com"}, Blocked: []string{"bad.example.com"}}
	for raw, code := range map[string]string{
		"https://example.com/a":                           "",
		"https://docs.example.com/a":                      "",
		"https://evil.com/a":                              "url_not_allowed",
		"https://bad.example.com/a":                       "url_not_allowed",
		"https://u:p@example.com/a":                       "url_not_allowed",
		"ftp://example.com/a":                             "invalid_tool_input",
		"not a url":                                       "invalid_tool_input",
		"https://example.com/" + strings.Repeat("a", 250): "url_too_long",
	} {
		_, err := checkURL(raw, cfg)
		got := ""
		if fe, ok := err.(*FetchError); ok {
			got = fe.Code
		}
		if got != code {
			t.Errorf("%.40s: code %q, want %q", raw, got, code)
		}
	}
}

func TestFetchRefusesThisMachine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	_, err := Fetch(context.Background(), srv.URL, FetchConfig{})
	if fe, ok := err.(*FetchError); !ok || fe.Code != "url_not_allowed" {
		t.Fatalf("a model fetched an address on this machine: %v", err)
	}
}

func allowLocal(t *testing.T) {
	old := dialControl
	dialControl = func(string, string, syscall.RawConn) error { return nil }
	t.Cleanup(func() { dialControl = old })
}

func TestFetch(t *testing.T) {
	allowLocal(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<head><title>T</title></head><p>hello</p>" + strings.Repeat("x", 100)))
	})
	mux.HandleFunc("/pdf", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "application/pdf") })
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"a":1}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f, err := Fetch(context.Background(), srv.URL+"/page", FetchConfig{})
	if err != nil || f.Title != "T" || !strings.HasPrefix(f.Text, "hello") {
		t.Fatalf("page: %+v, %v", f, err)
	}
	if f, _ = Fetch(context.Background(), srv.URL+"/page", FetchConfig{MaxTokens: 2}); len([]rune(f.Text)) != 8 {
		t.Errorf("max_content_tokens did not truncate: %d", len([]rune(f.Text)))
	}
	if f, err = Fetch(context.Background(), srv.URL+"/json", FetchConfig{}); err != nil || f.Text != `{"a":1}` {
		t.Errorf("json: %+v, %v", f, err)
	}
	for path, code := range map[string]string{"/pdf": "unsupported_content_type", "/gone": "url_not_accessible"} {
		_, err := Fetch(context.Background(), srv.URL+path, FetchConfig{})
		if fe, ok := err.(*FetchError); !ok || fe.Code != code {
			t.Errorf("%s: %v, want %s", path, err, code)
		}
	}
}

// mcpServer is a server of the tools part of MCP. sse answers in an event stream.
func mcpServer(t *testing.T, sse bool, token string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			result = obj{"protocolVersion": "2025-06-18", "capabilities": obj{}}
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "s1" {
				http.Error(w, "no session", http.StatusBadRequest)
				return
			}
			result = obj{"tools": []any{
				obj{"name": "echo", "description": "Echo text", "inputSchema": obj{"type": "object", "properties": obj{"text": obj{"type": "string"}}}},
				obj{"name": "boom", "description": "Always fails", "inputSchema": obj{"type": "object"}},
			}}
		case "tools/call":
			var p struct {
				Name string `json:"name"`
				Args obj    `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			if p.Name == "boom" {
				result = obj{"isError": true, "content": []any{obj{"type": "text", "text": "it broke"}}}
			} else {
				result = obj{"content": []any{obj{"type": "text", "text": "echo: " + str(p.Args["text"])}}}
			}
		}
		b, _ := json.Marshal(obj{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\nevent: message\ndata: " + string(b) + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
}

func TestMCP(t *testing.T) {
	for _, sse := range []bool{false, true} {
		srv := mcpServer(t, sse, "tok")
		c, err := ConnectMCP(context.Background(), srv.URL, "tok")
		if err != nil {
			t.Fatalf("sse=%v connect: %v", sse, err)
		}
		tools, err := c.ListTools(context.Background())
		if err != nil || len(tools) != 2 || tools[0].Name != "echo" {
			t.Fatalf("sse=%v tools: %v, %v", sse, tools, err)
		}
		text, isErr, err := c.Call(context.Background(), "echo", obj{"text": "hi"})
		if err != nil || isErr || text != "echo: hi" {
			t.Errorf("sse=%v call: %q %v %v", sse, text, isErr, err)
		}
		if text, isErr, _ = c.Call(context.Background(), "boom", nil); !isErr || text != "it broke" {
			t.Errorf("sse=%v tool error: %q %v", sse, text, isErr)
		}
		srv.Close()
	}
	srv := mcpServer(t, false, "tok")
	defer srv.Close()
	if _, err := ConnectMCP(context.Background(), srv.URL, "wrong"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a wrong token connected: %v", err)
	}
}

func pageServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><head><title>Page</title></head><body><p>fetched page body</p></body></html>"))
	}))
}
