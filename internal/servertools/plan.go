// Package servertools emulates the tools Anthropic runs on its own servers, for
// a model that is not Claude's: the MCP connector, tool search and web fetch. A
// model asks for one as it asks for any tool; intact runs it, gives the model
// the result and calls it again, then answers the client with the blocks
// Anthropic's API would have produced.
package servertools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type kind int

const (
	kindMCP kind = iota + 1
	kindSearch
	kindFetch
)

type handler struct {
	kind           kind
	server, remote string
}

// maxTurns bounds the calls intact makes to the model for one request. At the
// bound the answer ends with stop_reason pause_turn, which a client takes as a
// request to send the conversation back and go on.
const maxTurns = 10

// RequestError is a request that cannot be served, for the caller to see.
type RequestError struct{ Message string }

func (e *RequestError) Error() string { return e.Message }

// Plan is one request being served: the request as the model is sent it, the
// tools intact runs, and what the client will be answered with.
type Plan struct {
	// Stream is whether the client asked for a stream. The model is always
	// called without one; the finished message is what the client gets.
	Stream bool

	req     obj
	handled map[string]handler
	clients map[string]*MCP
	active  []any
	// deferred holds the tools that stay out of the request until a search
	// finds them, by name, in the order they were declared.
	deferred map[string]obj
	order    []string
	loaded   map[string]bool
	mcpNames map[string]string // server and remote name to the name the model sees

	searchName, searchKey string
	regex                 bool
	fetchName             string
	fetchCfg              FetchConfig
	fetchUses             int
	urls                  map[string]bool

	out     []any
	usage   map[string]int64
	server  map[string]int64
	tier    string
	turns   int
	started time.Time
}

// Wants reports whether a Messages request declares a tool that Plan runs.
func Wants(body []byte) bool {
	m, err := decode(body)
	if err != nil {
		return false
	}
	if len(list(m["mcp_servers"])) > 0 {
		return true
	}
	for _, t := range list(m["tools"]) {
		typ := str(asObj(t)["type"])
		if strings.HasPrefix(typ, "tool_search_tool_") || strings.HasPrefix(typ, "web_fetch") {
			return true
		}
	}
	return false
}

// Prepare reads a request into a Plan. It connects to the MCP servers the
// request names, to learn their tools.
func Prepare(ctx context.Context, body []byte) (*Plan, error) {
	m, err := decode(body)
	if err != nil {
		return nil, &RequestError{"the request is not JSON"}
	}
	p := &Plan{
		req: m, handled: map[string]handler{}, clients: map[string]*MCP{},
		deferred: map[string]obj{}, loaded: map[string]bool{}, mcpNames: map[string]string{},
		urls: map[string]bool{}, usage: map[string]int64{}, server: map[string]int64{},
		started: time.Now(),
	}
	p.Stream, _ = m["stream"].(bool)
	m["stream"] = false
	servers := list(m["mcp_servers"])
	delete(m, "mcp_servers")

	var clientTools []obj
	toolsets := map[string]obj{}
	searchOn := false
	for _, t := range list(m["tools"]) {
		td := asObj(t)
		typ := str(td["type"])
		switch {
		case strings.HasPrefix(typ, "tool_search_tool_"):
			searchOn = true
			p.searchName = str(td["name"])
			p.regex = strings.Contains(typ, "regex")
			p.searchKey = "query"
			if p.regex {
				p.searchKey = "pattern"
			}
			if p.searchName == "" {
				p.searchName = "tool_search_tool_" + map[bool]string{true: "regex", false: "bm25"}[p.regex]
			}
		case strings.HasPrefix(typ, "web_fetch"):
			p.fetchName = str(td["name"])
			if p.fetchName == "" {
				p.fetchName = "web_fetch"
			}
			p.fetchCfg = FetchConfig{
				MaxUses: int(number(td["max_uses"])), MaxTokens: int(number(td["max_content_tokens"])),
				Allowed: strs(td["allowed_domains"]), Blocked: strs(td["blocked_domains"]),
			}
		case typ == "mcp_toolset":
			toolsets[str(td["mcp_server_name"])] = td
		default:
			clientTools = append(clientTools, td)
		}
	}

	used := map[string]bool{p.searchName: searchOn, p.fetchName: p.fetchName != ""}
	for _, td := range clientTools {
		name := str(td["name"])
		used[name] = true
		deferred := isTrue(td["defer_loading"])
		delete(td, "defer_loading")
		if deferred && searchOn {
			p.addDeferred(name, td)
			continue
		}
		p.active = append(p.active, td)
	}

	for _, s := range servers {
		sv := asObj(s)
		name, url := str(sv["name"]), str(sv["url"])
		if name == "" || url == "" {
			return nil, &RequestError{"an MCP server needs a name and a url"}
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		client, err := ConnectMCP(cctx, url, str(sv["authorization_token"]))
		var tools []MCPTool
		if err == nil {
			tools, err = client.ListTools(cctx)
		}
		cancel()
		if err != nil {
			return nil, &RequestError{fmt.Sprintf("cannot reach MCP server %q: %v", name, err)}
		}
		p.clients[name] = client
		ts := toolsets[name]
		defCfg := asObj(ts["default_config"])
		cfgs := asObj(ts["configs"])
		for _, t := range tools {
			cfg := asObj(cfgs[t.Name])
			if !flag(cfg["enabled"], flag(defCfg["enabled"], true)) {
				continue
			}
			modelName := t.Name
			if used[modelName] {
				modelName = name + "__" + t.Name
			}
			used[modelName] = true
			p.handled[modelName] = handler{kind: kindMCP, server: name, remote: t.Name}
			p.mcpNames[name+"\x00"+t.Name] = modelName
			def := obj{"name": modelName, "description": t.Description, "input_schema": t.InputSchema}
			if searchOn && flag(cfg["defer_loading"], flag(defCfg["defer_loading"], false)) {
				p.addDeferred(modelName, def)
				continue
			}
			p.active = append(p.active, def)
		}
	}

	if searchOn {
		p.handled[p.searchName] = handler{kind: kindSearch}
	}
	if p.fetchName != "" {
		p.handled[p.fetchName] = handler{kind: kindFetch}
	}
	if p.fetchName != "" {
		p.active = append(p.active, obj{"name": p.fetchName, "description": fetchDescription,
			"input_schema": schema(obj{"url": obj{"type": "string", "description": "The URL of the page to fetch."}}, "url")})
	}

	if msgs, ok := m["messages"].([]any); ok {
		m["messages"] = p.normalize(msgs)
	}
	if searchOn && len(p.order) > 0 {
		p.active = append(p.active, p.searchTool())
	}
	p.rebuildTools()
	return p, nil
}

const fetchDescription = "Fetch the text of a web page. Only a URL that appears earlier in the conversation, " +
	"in a user message or a tool result, can be fetched."

func (p *Plan) addDeferred(name string, def obj) {
	p.deferred[name] = def
	p.order = append(p.order, name)
}

// searchTool is the tool the model searches the deferred ones with. The model of
// Anthropic knows it by training; any other is told what it does, and which
// tools it can find.
func (p *Plan) searchTool() obj {
	names := p.order
	more := ""
	if len(names) > 200 {
		names, more = names[:200], fmt.Sprintf(", and %d more", len(p.order)-200)
	}
	var desc, argDesc string
	if p.regex {
		desc = "Search for tools you have not been given yet. Give a case-insensitive regular expression; it is matched against each tool's name, description and arguments."
		argDesc = "A regular expression of at most 200 characters, such as \"get_.*_data\" or \"database.*query|query.*database\"."
	} else {
		desc = "Search for tools you have not been given yet. Describe what you need in natural language; the best matches are returned."
		argDesc = "A natural language query of at most 500 characters."
	}
	desc += " The tools it finds can be called right after. Tools available to search: " + strings.Join(names, ", ") + more + "."
	return obj{"name": p.searchName, "description": desc, "input_schema": schema(obj{
		p.searchKey: obj{"type": "string", "description": argDesc},
		"limit":     obj{"type": "integer", "description": "How many tools to return at most. Default 5."},
	}, p.searchKey)}
}

func (p *Plan) rebuildTools() {
	tools := append([]any(nil), p.active...)
	for _, n := range p.order {
		if p.loaded[n] {
			tools = append(tools, p.deferred[n])
		}
	}
	if len(tools) == 0 {
		delete(p.req, "tools")
		return
	}
	p.req["tools"] = tools
}

// Request is the request to send the model now.
func (p *Plan) Request() ([]byte, error) { return json.Marshal(p.req) }

// Advance takes the model's answer to the request. When the answer calls tools
// that intact runs, it runs them and returns the next request to send; when it
// does not, it returns the message to answer the client with.
func (p *Plan) Advance(ctx context.Context, answer []byte) (next []byte, final map[string]any, err error) {
	msg, err := decode(answer)
	if err != nil {
		return nil, nil, fmt.Errorf("the model's answer is not JSON: %w", err)
	}
	p.addUsage(asObj(msg["usage"]))
	content := list(msg["content"])
	var mine, client bool
	for _, b := range content {
		if blk := asObj(b); str(blk["type"]) == "tool_use" {
			if _, ok := p.handled[str(blk["name"])]; ok {
				mine = true
			} else {
				client = true
			}
		}
	}
	if !mine {
		p.out = append(p.out, content...)
		return nil, p.finish(msg, ""), nil
	}

	var internal, results []any
	for _, b := range content {
		blk := asObj(b)
		h, ok := p.handled[str(blk["name"])]
		if str(blk["type"]) != "tool_use" || !ok {
			p.out = append(p.out, b)
			internal = append(internal, b)
			continue
		}
		id := visibleID(h)
		call, result, text, isErr := p.run(ctx, h, id, blk["input"])
		p.out = append(p.out, call, result)
		internal = append(internal, obj{"type": "tool_use", "id": id, "name": blk["name"], "input": blk["input"]})
		r := obj{"type": "tool_result", "tool_use_id": id, "content": text}
		if isErr {
			r["is_error"] = true
		}
		results = append(results, r)
	}
	if client {
		// The client runs its own tools, then sends the conversation back.
		return nil, p.finish(msg, ""), nil
	}
	if p.turns++; p.turns >= maxTurns {
		return nil, p.finish(msg, "pause_turn"), nil
	}
	p.req["messages"] = append(list(p.req["messages"]),
		obj{"role": "assistant", "content": internal}, obj{"role": "user", "content": results})
	p.afterFirstTurn()
	p.rebuildTools()
	next, err = json.Marshal(p.req)
	return next, nil, err
}

// afterFirstTurn drops what applied to the first call only: a tool forced by
// tool_choice would be forced again at every turn.
func (p *Plan) afterFirstTurn() {
	if tc := asObj(p.req["tool_choice"]); str(tc["type"]) == "any" || str(tc["type"]) == "tool" {
		delete(p.req, "tool_choice")
	}
}

// Preface puts a web search that intact ran before the first call at the head of
// the answer. The URLs it found may be fetched from then on, as they may be in
// Anthropic's API.
func (p *Plan) Preface(call, result map[string]any, searches int) {
	p.out = append(p.out, call, result)
	p.server["web_search_requests"] += int64(searches)
	p.harvestSearch(result)
}

func (p *Plan) harvestSearch(result obj) {
	for _, it := range list(result["content"]) {
		if u := str(asObj(it)["url"]); u != "" {
			p.urls[trimURL(u)] = true
		}
	}
}

func visibleID(h handler) string {
	if h.kind == kindMCP {
		return newID("mcptoolu_")
	}
	return newID("srvtoolu_")
}

func (p *Plan) finish(msg obj, stop string) obj {
	out := p.out
	if out == nil {
		out = []any{}
	}
	if stop == "" {
		stop, _ = msg["stop_reason"].(string)
	}
	usage := obj{}
	for k, v := range p.usage {
		usage[k] = v
	}
	if p.tier != "" {
		usage["service_tier"] = p.tier
	}
	if len(p.server) > 0 {
		s := obj{}
		for k, v := range p.server {
			s[k] = v
		}
		usage["server_tool_use"] = s
	}
	return obj{"id": msg["id"], "type": "message", "role": "assistant", "model": msg["model"],
		"content": out, "stop_reason": stop, "stop_sequence": msg["stop_sequence"], "usage": usage}
}

func (p *Plan) addUsage(u obj) {
	for k, v := range u {
		switch x := v.(type) {
		case json.Number:
			p.usage[k] += number(x)
		case string:
			if k == "service_tier" {
				p.tier = x
			}
		case map[string]any:
			if k == "server_tool_use" {
				for n, c := range x {
					p.server[n] += number(c)
				}
			}
		}
	}
}

// run executes one call of a tool of intact's. It returns the two blocks the
// client is shown, and the text and error flag the model is given back.
func (p *Plan) run(ctx context.Context, h handler, id string, input any) (call, result obj, text string, isErr bool) {
	in := asObj(input)
	if in == nil {
		in = obj{}
	}
	switch h.kind {
	case kindMCP:
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		var err error
		text, isErr, err = p.clients[h.server].Call(cctx, h.remote, in)
		if err != nil {
			text, isErr = "Error: "+err.Error(), true
		}
		call = obj{"type": "mcp_tool_use", "id": id, "name": h.remote, "server_name": h.server, "input": in}
		result = obj{"type": "mcp_tool_result", "tool_use_id": id, "is_error": isErr,
			"content": []any{obj{"type": "text", "text": text}}}
	case kindSearch:
		call = obj{"type": "server_tool_use", "id": id, "name": p.searchName, "input": in}
		result, text, isErr = p.runSearch(id, in)
	case kindFetch:
		call = obj{"type": "server_tool_use", "id": id, "name": p.fetchName, "input": in}
		result, text, isErr = p.runFetch(ctx, id, in)
	}
	return call, result, text, isErr
}

func (p *Plan) runSearch(id string, in obj) (result obj, text string, isErr bool) {
	limit := int(number(in["limit"]))
	if limit <= 0 {
		limit = defaultHits
	}
	items := make([]searchable, 0, len(p.order))
	for _, n := range p.order {
		items = append(items, searchableOf(p.deferred[n]))
	}
	var hits []string
	var err error
	if p.regex {
		hits, err = regexSearch(items, str(in[p.searchKey]), limit)
	} else {
		hits, err = bm25Search(items, str(in[p.searchKey]), limit)
	}
	if err != nil {
		code := "invalid_tool_input"
		if te, ok := err.(*toolSearchError); ok {
			code = te.code
		}
		return obj{"type": "tool_search_tool_result", "tool_use_id": id, "content": obj{
			"type": "tool_search_tool_result_error", "error_code": code, "error_message": err.Error()}}, "Error: " + err.Error(), true
	}
	refs := []any{}
	for _, n := range hits {
		p.loaded[n] = true
		refs = append(refs, obj{"type": "tool_reference", "tool_name": n})
	}
	text = "No tools matched."
	if len(hits) > 0 {
		text = "Found tools: " + strings.Join(hits, ", ") + ". They can be called now."
	}
	return obj{"type": "tool_search_tool_result", "tool_use_id": id, "content": obj{
		"type": "tool_search_tool_search_result", "tool_references": refs}}, text, false
}

func (p *Plan) runFetch(ctx context.Context, id string, in obj) (result obj, text string, isErr bool) {
	p.server["web_fetch_requests"]++
	fail := func(code string) (obj, string, bool) {
		return obj{"type": "web_fetch_tool_result", "tool_use_id": id, "content": obj{
			"type": "web_fetch_tool_result_error", "error_code": code}}, "Error: " + code, true
	}
	raw := strings.TrimSpace(str(in["url"]))
	if p.fetchUses++; p.fetchCfg.MaxUses > 0 && p.fetchUses > p.fetchCfg.MaxUses {
		return fail("max_uses_exceeded")
	}
	if raw == "" {
		return fail("invalid_tool_input")
	}
	if !p.urls[trimURL(raw)] {
		return fail("url_not_in_prior_context")
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	f, err := Fetch(fctx, raw, p.fetchCfg)
	if err != nil {
		code := "unavailable"
		if fe, ok := err.(*FetchError); ok {
			code = fe.Code
		}
		return fail(code)
	}
	p.urls[trimURL(f.URL)] = true
	doc := obj{"type": "document", "source": obj{"type": "text", "media_type": "text/plain", "data": f.Text}}
	if f.Title != "" {
		doc["title"] = f.Title
	}
	return obj{"type": "web_fetch_tool_result", "tool_use_id": id, "content": obj{
		"type": "web_fetch_result", "url": f.URL, "content": doc,
		"retrieved_at": f.At.Format(time.RFC3339)}}, f.modelText(), false
}

// ---- history

var reURL = regexp.MustCompile(`https?://[^\s<>"'\x60)\]]+`)

func trimURL(u string) string { return strings.TrimRight(u, ".,;:!?") }

func (p *Plan) harvest(s string) {
	for _, u := range reURL.FindAllString(s, -1) {
		p.urls[trimURL(u)] = true
	}
}

// normalize rewrites the history the client sends back into plain tool calls. A
// turn that intact answered carries the call and its result in one assistant
// message, as blocks no other model reads; each pair becomes a call and a result
// in turn, which every provider takes. It also learns from the history what the
// tools need: the URLs a fetch may use, and the tools a search already found.
func (p *Plan) normalize(msgs []any) []any {
	var out []obj
	for _, raw := range msgs {
		m := asObj(raw)
		blocks, isList := m["content"].([]any)
		if !isList {
			if str(m["role"]) == "user" {
				p.harvest(str(m["content"]))
			}
			out = append(out, m)
			continue
		}
		if str(m["role"]) != "assistant" {
			out = append(out, obj{"role": m["role"], "content": p.userBlocks(blocks)})
			continue
		}
		var cur []any
		flush := func() {
			if len(cur) > 0 {
				out = append(out, obj{"role": "assistant", "content": cur})
				cur = nil
			}
		}
		for _, b := range blocks {
			blk := asObj(b)
			switch typ := str(blk["type"]); typ {
			case "server_tool_use":
				if _, ok := p.handled[str(blk["name"])]; ok {
					cur = append(cur, obj{"type": "tool_use", "id": blk["id"], "name": blk["name"], "input": blk["input"]})
					continue
				}
			case "web_search_tool_result":
				p.harvestSearch(blk)
			case "mcp_tool_use":
				name := str(blk["name"])
				if n, ok := p.mcpNames[str(blk["server_name"])+"\x00"+name]; ok {
					name = n
				}
				cur = append(cur, obj{"type": "tool_use", "id": blk["id"], "name": name, "input": blk["input"]})
				continue
			case "mcp_tool_result", "tool_search_tool_result", "web_fetch_tool_result":
				flush()
				res := obj{"type": "tool_result", "tool_use_id": blk["tool_use_id"], "content": p.resultText(typ, blk)}
				if e := isTrue(blk["is_error"]); e || isErrorResult(blk) {
					res["is_error"] = true
				}
				out = append(out, obj{"role": "user", "content": []any{res}})
				continue
			}
			cur = append(cur, b)
		}
		flush()
	}
	return mergeUsers(out)
}

// userBlocks reads a user turn: the URLs in it may be fetched, and a tool result
// that names tools (a client-side search) loads them.
func (p *Plan) userBlocks(blocks []any) []any {
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		blk := asObj(b)
		switch str(blk["type"]) {
		case "text":
			p.harvest(str(blk["text"]))
		case "tool_result":
			p.harvest(textOf(blk["content"]))
			if l, ok := blk["content"].([]any); ok && hasReference(l) {
				blk = copyObj(blk)
				blk["content"] = p.expandReferences(l)
				b = blk
			}
		}
		out = append(out, b)
	}
	return out
}

func hasReference(l []any) bool {
	for _, c := range l {
		if str(asObj(c)["type"]) == "tool_reference" {
			return true
		}
	}
	return false
}

// expandReferences turns the tool_reference blocks of a client's search result
// into text, which a provider reads, and loads the tools they name.
func (p *Plan) expandReferences(l []any) []any {
	out := make([]any, 0, len(l))
	for _, c := range l {
		cb := asObj(c)
		if str(cb["type"]) != "tool_reference" {
			out = append(out, c)
			continue
		}
		name := str(cb["tool_name"])
		if _, ok := p.deferred[name]; ok {
			p.loaded[name] = true
		}
		out = append(out, obj{"type": "text", "text": "Loaded tool: " + name})
	}
	return out
}

func isErrorResult(blk obj) bool {
	c := asObj(blk["content"])
	return strings.HasSuffix(str(c["type"]), "_error")
}

// resultText is what the model is told a result of intact's said, and records
// what it found: the tools a search returned, the URL a fetch read.
func (p *Plan) resultText(typ string, blk obj) string {
	switch typ {
	case "mcp_tool_result":
		return textOf(blk["content"])
	case "tool_search_tool_result":
		c := asObj(blk["content"])
		if strings.HasSuffix(str(c["type"]), "_error") {
			return "Error: " + str(c["error_message"])
		}
		var names []string
		for _, r := range list(c["tool_references"]) {
			n := str(asObj(r)["tool_name"])
			if _, ok := p.deferred[n]; ok {
				p.loaded[n] = true
			}
			names = append(names, n)
		}
		if len(names) == 0 {
			return "No tools matched."
		}
		return "Found tools: " + strings.Join(names, ", ") + ". They can be called now."
	default: // web_fetch_tool_result
		c := asObj(blk["content"])
		if strings.HasSuffix(str(c["type"]), "_error") {
			return "Error: " + str(c["error_code"])
		}
		p.urls[trimURL(str(c["url"]))] = true
		doc := asObj(c["content"])
		f := Fetched{URL: str(c["url"]), Title: str(doc["title"]), Text: str(asObj(doc["source"])["data"])}
		return f.modelText()
	}
}

// mergeUsers joins user turns that follow each other, which splitting an
// assistant turn can leave, so that no provider sees two in a row.
func mergeUsers(msgs []obj) []any {
	var out []any
	var last obj
	for _, m := range msgs {
		if last != nil && str(last["role"]) == "user" && str(m["role"]) == "user" {
			last["content"] = append(asBlocks(last["content"]), asBlocks(m["content"])...)
			continue
		}
		last = m
		out = append(out, m)
	}
	return out
}

func asBlocks(content any) []any {
	if s, ok := content.(string); ok {
		return []any{obj{"type": "text", "text": s}}
	}
	return list(content)
}

func copyObj(o obj) obj {
	c := make(obj, len(o))
	for k, v := range o {
		c[k] = v
	}
	return c
}

func strs(v any) []string {
	var out []string
	for _, s := range list(v) {
		if x := str(s); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func flag(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}
