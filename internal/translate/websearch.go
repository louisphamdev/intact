package translate

// WebSearch is a search intact ran because the caller asked its model for one:
// Claude Code's WebSearch tool declares Anthropic's hosted tool, and a provider
// that is not Anthropic has none. The answer carries what Anthropic's servers
// would have put in it, so the client reads the sources from where it expects
// them.
type WebSearch struct {
	Query   string
	Results []WebResult
	// Failure is why the search did not run; the client is then told it is
	// unavailable and the results are empty.
	Failure string
}

// WebResult is one page the search found.
type WebResult struct {
	Title, URL, Snippet, Age string
}

// searchBlocks builds the two blocks that open the answer: the model's call of
// the hosted tool, and the result of it.
func (s *WebSearch) blocks() (call, result obj) {
	id := newID("srvtoolu_")
	call = obj{"type": "server_tool_use", "id": id, "name": "web_search", "input": obj{"query": s.Query}}
	var content any
	if s.Failure != "" {
		content = obj{"type": "web_search_tool_result_error", "error_code": "unavailable"}
	} else {
		items := []any{}
		for _, r := range s.Results {
			var age any
			if r.Age != "" {
				age = r.Age
			}
			items = append(items, obj{"type": "web_search_result", "url": r.URL, "title": r.Title,
				"encrypted_content": r.Snippet, "page_age": age})
		}
		content = items
	}
	result = obj{"type": "web_search_tool_result", "tool_use_id": id, "content": content}
	return call, result
}

// requests is how many searches the answer reports in its usage.
func (s *WebSearch) requests() int {
	if s.Failure != "" {
		return 0
	}
	return 1
}

// withSearchUsage adds the count of searches run to a usage object.
func withSearchUsage(u obj, s *WebSearch) obj {
	if s != nil {
		u["server_tool_use"] = obj{"web_search_requests": s.requests()}
	}
	return u
}

// Blocks returns the call and the result of the search as the client sees them,
// for an answer that intact assembles itself.
func (s *WebSearch) Blocks() (call, result map[string]any) { return s.blocks() }

// Requests is how many searches the answer reports in its usage.
func (s *WebSearch) Requests() int { return s.requests() }
