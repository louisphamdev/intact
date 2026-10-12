package servertools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func answer(t *testing.T, stop string, blocks ...obj) []byte {
	t.Helper()
	content := make([]any, len(blocks))
	for i, b := range blocks {
		content[i] = b
	}
	return mustJSON(t, obj{"id": "msg_1", "type": "message", "role": "assistant", "model": "m", "content": content,
		"stop_reason": stop, "stop_sequence": nil, "usage": obj{"input_tokens": 10, "output_tokens": 5}})
}

func toolUse(id, name string, input obj) obj {
	return obj{"type": "tool_use", "id": id, "name": name, "input": input}
}

func toolNames(t *testing.T, req []byte) []string {
	t.Helper()
	m, _ := decode(req)
	var names []string
	for _, tl := range list(m["tools"]) {
		names = append(names, str(asObj(tl)["name"]))
	}
	return names
}

func has(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

func types(blocks []any) string {
	var ts []string
	for _, b := range blocks {
		ts = append(ts, str(asObj(b)["type"]))
	}
	return strings.Join(ts, ",")
}

func scenarioBody(t *testing.T, mcpURL, pageURL string) []byte {
	return mustJSON(t, obj{
		"model": "m", "max_tokens": 100, "stream": true,
		"mcp_servers": []any{obj{"type": "url", "url": mcpURL, "name": "demo", "authorization_token": "tok"}},
		"tools": []any{
			obj{"type": "mcp_toolset", "mcp_server_name": "demo",
				"default_config": obj{"defer_loading": true}, "configs": obj{"echo": obj{"defer_loading": false}}},
			obj{"type": "tool_search_tool_regex_20251119", "name": "tool_search_tool_regex"},
			obj{"type": "web_fetch_20250910", "name": "web_fetch", "max_uses": 5},
			obj{"name": "Read", "description": "read a file", "input_schema": obj{"type": "object"}},
		},
		"messages": []any{obj{"role": "user", "content": "read " + pageURL + " please"}},
	})
}

func TestPlanRunsTheServerTools(t *testing.T) {
	allowLocal(t)
	mcp := mcpServer(t, false, "tok")
	defer mcp.Close()
	page := pageServer()
	defer page.Close()
	ctx := context.Background()

	if !Wants(scenarioBody(t, mcp.URL, page.URL)) || Wants([]byte(`{"tools":[{"name":"x","input_schema":{}}]}`)) {
		t.Fatal("Wants misjudges a request")
	}
	p, err := Prepare(ctx, scenarioBody(t, mcp.URL, page.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Stream {
		t.Error("the client's stream flag was lost")
	}
	req, _ := p.Request()
	m, _ := decode(req)
	if m["stream"] != false || m["mcp_servers"] != nil {
		t.Errorf("the model must be called without a stream and without mcp_servers: %s", req)
	}
	names := toolNames(t, req)
	for _, want := range []string{"Read", "echo", "tool_search_tool_regex", "web_fetch"} {
		if !has(names, want) {
			t.Errorf("tool %s missing from %v", want, names)
		}
	}
	if has(names, "boom") {
		t.Error("a deferred tool was sent before a search found it")
	}
	for _, tl := range list(m["tools"]) {
		td := asObj(tl)
		if td["type"] != nil || td["input_schema"] == nil {
			t.Errorf("tool %v is not a plain function tool", td["name"])
		}
		if str(td["name"]) == "tool_search_tool_regex" && !strings.Contains(str(td["description"]), "boom") {
			t.Error("the search tool does not say what it can find")
		}
	}

	// 1. the model calls an MCP tool
	next, final, err := p.Advance(ctx, answer(t, "tool_use", obj{"type": "text", "text": "calling"}, toolUse("toolu_a", "echo", obj{"text": "hi"})))
	if err != nil || final != nil || next == nil {
		t.Fatalf("turn 1: %v %v", final, err)
	}
	nm, _ := decode(next)
	msgs := list(nm["messages"])
	if len(msgs) != 3 || str(asObj(msgs[1])["role"]) != "assistant" || str(asObj(msgs[2])["role"]) != "user" {
		t.Fatalf("the exchange was not folded into the request: %s", next)
	}
	res := asObj(list(asObj(msgs[2])["content"])[0])
	if res["type"] != "tool_result" || res["content"] != "echo: hi" {
		t.Errorf("tool_result = %v", res)
	}
	if asObj(list(asObj(msgs[1])["content"])[1])["id"] == "toolu_a" {
		t.Error("the call kept the model's id instead of the one the client sees")
	}

	// 2. the model searches for a tool, and gets it the next turn
	next, _, _ = p.Advance(ctx, answer(t, "tool_use", toolUse("toolu_b", "tool_search_tool_regex", obj{"pattern": "BOOM"})))
	if !has(toolNames(t, next), "boom") {
		t.Errorf("the tool a search found is not offered: %v", toolNames(t, next))
	}

	// 3. a fetch of a URL the conversation never mentioned is refused; one it did is served
	next, _, _ = p.Advance(ctx, answer(t, "tool_use", toolUse("toolu_c", "web_fetch", obj{"url": "https://unseen.example/x"})))
	nm, _ = decode(next)
	last := asObj(list(asObj(list(nm["messages"])[len(list(nm["messages"]))-1])["content"])[0])
	if !strings.Contains(str(last["content"]), "url_not_in_prior_context") || last["is_error"] != true {
		t.Errorf("fetch of an unseen URL: %v", last)
	}
	next, _, _ = p.Advance(ctx, answer(t, "tool_use", toolUse("toolu_d", "web_fetch", obj{"url": page.URL})))
	nm, _ = decode(next)
	last = asObj(list(asObj(list(nm["messages"])[len(list(nm["messages"]))-1])["content"])[0])
	if !strings.Contains(str(last["content"]), "fetched page body") {
		t.Errorf("fetch result = %v", last)
	}

	// 4. the model answers
	next, final, err = p.Advance(ctx, answer(t, "end_turn", obj{"type": "text", "text": "done"}))
	if err != nil || next != nil || final == nil {
		t.Fatalf("final: %v %v", final, err)
	}
	want := "text,mcp_tool_use,mcp_tool_result,server_tool_use,tool_search_tool_result," +
		"server_tool_use,web_fetch_tool_result,server_tool_use,web_fetch_tool_result,text"
	if got := types(list(final["content"])); got != want {
		t.Errorf("blocks:\n got %s\nwant %s", got, want)
	}
	if final["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", final["stop_reason"])
	}
	u := asObj(final["usage"])
	if u["input_tokens"] != int64(50) || u["output_tokens"] != int64(25) || asObj(u["server_tool_use"])["web_fetch_requests"] != int64(2) {
		t.Errorf("usage = %v", u)
	}
	blocks := list(final["content"])
	mc := asObj(blocks[1])
	if mc["name"] != "echo" || mc["server_name"] != "demo" || !strings.HasPrefix(str(mc["id"]), "mcptoolu_") {
		t.Errorf("mcp_tool_use = %v", mc)
	}
	if asObj(blocks[2])["tool_use_id"] != mc["id"] {
		t.Error("the result does not answer its call")
	}
	sr := asObj(asObj(blocks[4])["content"])
	if sr["type"] != "tool_search_tool_search_result" || asObj(list(sr["tool_references"])[0])["tool_name"] != "boom" {
		t.Errorf("tool_search_tool_result = %v", sr)
	}
	if e := asObj(asObj(blocks[6])["content"]); e["error_code"] != "url_not_in_prior_context" {
		t.Errorf("fetch error block = %v", e)
	}
	if ok := asObj(asObj(blocks[8])["content"]); ok["type"] != "web_fetch_result" || asObj(asObj(ok["content"])["source"])["media_type"] != "text/plain" {
		t.Errorf("web_fetch_result = %v", ok)
	}

	// the stream the client asked for
	events := sseEvents(t, MessageSSE(final))
	if events[0] != "message_start" || events[len(events)-2] != "message_delta" || events[len(events)-1] != "message_stop" {
		t.Errorf("events = %v", events)
	}
	if c := strings.Count(strings.Join(events, ","), "content_block_start"); c != len(blocks) {
		t.Errorf("%d content_block_start for %d blocks", c, len(blocks))
	}
}

func TestPlanMixedTurnGoesToTheClient(t *testing.T) {
	mcp := mcpServer(t, false, "tok")
	defer mcp.Close()
	p, err := Prepare(context.Background(), scenarioBody(t, mcp.URL, "https://example.com"))
	if err != nil {
		t.Fatal(err)
	}
	next, final, err := p.Advance(context.Background(), answer(t, "tool_use",
		toolUse("toolu_a", "echo", obj{"text": "x"}), toolUse("toolu_r", "Read", obj{"path": "/a"})))
	if err != nil || next != nil || final == nil {
		t.Fatalf("%v %v", final, err)
	}
	if got := types(list(final["content"])); got != "mcp_tool_use,mcp_tool_result,tool_use" {
		t.Errorf("blocks = %s", got)
	}
	if final["stop_reason"] != "tool_use" || asObj(list(final["content"])[2])["id"] != "toolu_r" {
		t.Errorf("the client's own call must go back as the model made it: %v", final)
	}
}

func TestPlanPausesAtTheTurnLimit(t *testing.T) {
	mcp := mcpServer(t, false, "tok")
	defer mcp.Close()
	p, _ := Prepare(context.Background(), scenarioBody(t, mcp.URL, "https://example.com"))
	var final obj
	for i := 0; i < maxTurns+2 && final == nil; i++ {
		_, final, _ = p.Advance(context.Background(), answer(t, "tool_use", toolUse("t", "echo", obj{"text": "x"})))
	}
	if final == nil || final["stop_reason"] != "pause_turn" || p.turns != maxTurns {
		t.Fatalf("turns=%d final=%v", p.turns, final)
	}
}

func TestPlanReadsTheHistory(t *testing.T) {
	allowLocal(t)
	mcp := mcpServer(t, false, "tok")
	defer mcp.Close()
	var body obj
	json.Unmarshal(scenarioBody(t, mcp.URL, "https://example.com"), &body)
	body["messages"] = []any{
		obj{"role": "user", "content": "start"},
		obj{"role": "assistant", "content": []any{
			obj{"type": "text", "text": "searching"},
			obj{"type": "server_tool_use", "id": "srvtoolu_1", "name": "tool_search_tool_regex", "input": obj{"pattern": "boom"}},
			obj{"type": "tool_search_tool_result", "tool_use_id": "srvtoolu_1", "content": obj{
				"type": "tool_search_tool_search_result", "tool_references": []any{obj{"type": "tool_reference", "tool_name": "boom"}}}},
			obj{"type": "mcp_tool_use", "id": "mcptoolu_1", "name": "echo", "server_name": "demo", "input": obj{"text": "a"}},
			obj{"type": "mcp_tool_result", "tool_use_id": "mcptoolu_1", "is_error": false, "content": []any{obj{"type": "text", "text": "echo: a"}}},
			obj{"type": "server_tool_use", "id": "srvtoolu_2", "name": "web_fetch", "input": obj{"url": "https://seen.example/p"}},
			obj{"type": "web_fetch_tool_result", "tool_use_id": "srvtoolu_2", "content": obj{
				"type": "web_fetch_result", "url": "https://seen.example/p", "content": obj{
					"type": "document", "title": "Seen", "source": obj{"type": "text", "media_type": "text/plain", "data": "page words"}}}},
			obj{"type": "text", "text": "now a client tool"},
			obj{"type": "tool_use", "id": "toolu_r", "name": "Read", "input": obj{"path": "/a"}},
		}},
		obj{"role": "user", "content": []any{obj{"type": "tool_result", "tool_use_id": "toolu_r", "content": "file text"}}},
	}
	p, err := Prepare(context.Background(), mustJSON(t, body))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := p.Request()
	m, _ := decode(req)
	msgs := list(m["messages"])
	var shape []string
	for _, raw := range msgs {
		msg := asObj(raw)
		shape = append(shape, str(msg["role"])+":"+types(asBlocks(msg["content"])))
	}
	want := []string{
		"user:text",
		"assistant:text,tool_use", "user:tool_result",
		"assistant:tool_use", "user:tool_result",
		"assistant:tool_use", "user:tool_result",
		"assistant:text,tool_use", "user:tool_result",
	}
	if strings.Join(shape, " | ") != strings.Join(want, " | ") {
		t.Fatalf("history:\n got %v\nwant %v", shape, want)
	}
	if !has(toolNames(t, req), "boom") {
		t.Error("a tool found in an earlier turn was not offered again")
	}
	fetched := asObj(list(asObj(msgs[6])["content"])[0])
	if !strings.Contains(str(fetched["content"]), "page words") || !strings.Contains(str(fetched["content"]), "Seen") {
		t.Errorf("the model must still see what it fetched: %v", fetched)
	}
	if !p.urls["https://seen.example/p"] {
		t.Error("a URL that an earlier fetch read may be fetched again")
	}
	if asObj(list(asObj(msgs[3])["content"])[0])["name"] != "echo" {
		t.Error("mcp_tool_use lost its tool name")
	}
}

func TestPlanUnreachableServer(t *testing.T) {
	_, err := Prepare(context.Background(), scenarioBody(t, "http://127.0.0.1:1/mcp", "https://example.com"))
	if re, ok := err.(*RequestError); !ok || !strings.Contains(re.Message, `"demo"`) {
		t.Errorf("err = %v", err)
	}
}

func TestPlanToolNameClash(t *testing.T) {
	mcp := mcpServer(t, false, "tok")
	defer mcp.Close()
	var body obj
	json.Unmarshal(scenarioBody(t, mcp.URL, "https://example.com"), &body)
	body["tools"] = append(list(body["tools"]), obj{"name": "echo", "description": "mine", "input_schema": obj{"type": "object"}})
	p, err := Prepare(context.Background(), mustJSON(t, body))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := p.Request()
	if names := toolNames(t, req); !has(names, "demo__echo") || !has(names, "echo") {
		t.Errorf("the MCP tool must not shadow a client tool: %v", names)
	}
}

func TestPlanBM25Search(t *testing.T) {
	mcp := mcpServer(t, false, "tok")
	defer mcp.Close()
	var body obj
	json.Unmarshal(scenarioBody(t, mcp.URL, "https://example.com"), &body)
	body["tools"] = []any{
		obj{"type": "mcp_toolset", "mcp_server_name": "demo", "default_config": obj{"defer_loading": true}},
		obj{"type": "tool_search_tool_bm25_20251119", "name": "tool_search_tool_bm25"},
		obj{"name": "Read", "input_schema": obj{"type": "object"}},
	}
	p, err := Prepare(context.Background(), mustJSON(t, body))
	if err != nil {
		t.Fatal(err)
	}
	next, _, _ := p.Advance(context.Background(), answer(t, "tool_use", toolUse("t", "tool_search_tool_bm25", obj{"query": "something that always fails"})))
	if !has(toolNames(t, next), "boom") {
		t.Errorf("bm25 found nothing: %v", toolNames(t, next))
	}
}

func sseEvents(t *testing.T, b []byte) []string {
	t.Helper()
	var events []string
	for _, block := range strings.Split(strings.TrimSpace(string(b)), "\n\n") {
		lines := strings.SplitN(block, "\n", 2)
		name := strings.TrimPrefix(lines[0], "event: ")
		var d obj
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &d); err != nil {
			t.Fatalf("event %q is not JSON: %v", name, err)
		}
		if d["type"] != name {
			t.Errorf("event %q carries type %v", name, d["type"])
		}
		events = append(events, name)
	}
	return events
}

func TestAFetchMayUseAURLASearchFound(t *testing.T) {
	allowLocal(t)
	page := pageServer()
	defer page.Close()
	body := mustJSON(t, obj{"model": "m", "max_tokens": 10,
		"tools":    []any{obj{"type": "web_fetch_20250910", "name": "web_fetch"}},
		"messages": []any{obj{"role": "user", "content": "find and read the Go release notes"}}})
	p, err := Prepare(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	// Without the search, the URL is out of the conversation.
	_, _, text, isErr := p.run(context.Background(), p.handled["web_fetch"], "x", obj{"url": page.URL})
	if !isErr || !strings.Contains(text, "url_not_in_prior_context") {
		t.Fatalf("a URL nobody mentioned was fetched: %q", text)
	}
	p.Preface(obj{"type": "server_tool_use", "id": "s", "name": "web_search", "input": obj{"query": "q"}},
		obj{"type": "web_search_tool_result", "tool_use_id": "s", "content": []any{obj{"type": "web_search_result", "url": page.URL, "title": "T"}}}, 1)
	if _, _, text, isErr = p.run(context.Background(), p.handled["web_fetch"], "y", obj{"url": page.URL}); isErr || !strings.Contains(text, "fetched page body") {
		t.Errorf("a URL the search found was refused: %q", text)
	}
	_, final, _ := p.Advance(context.Background(), answer(t, "end_turn", obj{"type": "text", "text": "ok"}))
	if got := types(list(final["content"])); got != "server_tool_use,web_search_tool_result,text" {
		t.Errorf("blocks = %s", got)
	}
	if asObj(final["usage"])["server_tool_use"].(obj)["web_search_requests"] != int64(1) {
		t.Errorf("usage = %v", final["usage"])
	}

	// On a later request the search result is in the history.
	var next obj
	json.Unmarshal(body, &next)
	next["messages"] = []any{obj{"role": "user", "content": "go on"}, obj{"role": "assistant", "content": []any{
		obj{"type": "server_tool_use", "id": "s", "name": "web_search", "input": obj{"query": "q"}},
		obj{"type": "web_search_tool_result", "tool_use_id": "s", "content": []any{obj{"type": "web_search_result", "url": "https://found.example/x"}}},
		obj{"type": "text", "text": "I found a page"}}}, obj{"role": "user", "content": "read it"}}
	p2, err := Prepare(context.Background(), mustJSON(t, next))
	if err != nil || !p2.urls["https://found.example/x"] {
		t.Errorf("a URL from an earlier search was forgotten: %v", err)
	}
}

func TestTheStreamOpensAThinkingBlockWithAnEmptySignature(t *testing.T) {
	out := string(MessageSSE(obj{"id": "m", "model": "x", "stop_reason": "end_turn", "usage": obj{},
		"content": []any{obj{"type": "thinking", "thinking": "hm", "signature": ""}}}))
	if !strings.Contains(out, `"content_block":{"signature":"","thinking":"","type":"thinking"}`) {
		t.Errorf("thinking block opens as: %s", out)
	}
	if strings.Contains(out, "signature_delta") {
		t.Errorf("an empty signature was sent as a delta: %s", out)
	}
}
