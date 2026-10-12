package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/contract"
	"github.com/louisphamdev/intact/internal/servertools"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/translate"
	"github.com/louisphamdev/intact/internal/websearch"
)

// webState holds the search service intact calls for a client that asks its model
// to search. It is built from the environment on first use, so an intact with no
// search configured costs nothing.
type webState struct {
	once sync.Once
	s    websearch.Searcher
	// unavailable is why there is no searcher.
	unavailable string
}

// searcher returns the search service to use, or why there is none. A service set
// in the environment wins. With none set, an active Antigravity account searches
// through Google, so a setup that already has one needs nothing more.
func (a *api) searcher() (websearch.Searcher, string) {
	a.web.once.Do(func() {
		if a.web.s != nil { // set by a test
			return
		}
		provider := strings.ToLower(strings.TrimSpace(os.Getenv("INTACT_SEARCH_PROVIDER")))
		switch provider {
		case "":
			a.web.unavailable = "web search is not set up on this intact: add an Antigravity account, or set INTACT_SEARCH_PROVIDER and INTACT_SEARCH_KEY"
		case "antigravity":
			a.web.s = antigravitySearch{a: a}
		default:
			s, err := websearch.New(*websearch.FromEnv())
			if err != nil {
				a.web.unavailable = err.Error()
				return
			}
			a.web.s = s
		}
	})
	if a.web.s == nil && a.web.unavailable != "" && os.Getenv("INTACT_SEARCH_PROVIDER") == "" && len(a.activeConnections("antigravity")) > 0 {
		return antigravitySearch{a: a}, ""
	}
	return a.web.s, a.web.unavailable
}

// lazySearch is the search service handed to another face of intact (the Dia
// bridge). It asks searcher on every search instead of once at startup: an
// Antigravity account added after intact started is then used, exactly as /v1
// would use it, and this intact with no search configured reads nothing up front.
type lazySearch struct{ a *api }

func (s lazySearch) Search(ctx context.Context, query string, count int) ([]websearch.Result, error) {
	svc, why := s.a.searcher()
	if svc == nil {
		return nil, errors.New(why)
	}
	return svc.Search(ctx, query, count)
}

// hostedSearch answers a request that declares Anthropic's hosted web search
// tool, for a provider that has none: it runs the search, and returns the request
// rewritten for the model with the results in it, and the search for the answer to
// carry. A request it cannot rewrite is returned as it came, with no search.
func (a *api) hostedSearch(ctx context.Context, body []byte) ([]byte, *translate.WebSearch) {
	query := websearch.Query(body)
	var results []websearch.Result
	failure := ""
	switch s, why := a.searcher(); {
	case query == "":
		failure = "the request names nothing to search for"
	case s == nil:
		failure = why
	default:
		var err error
		if results, err = s.Search(ctx, query, 0); err != nil {
			failure = err.Error()
		}
	}
	rewritten, err := websearch.Rewrite(body, query, results, failure)
	if err != nil {
		return body, nil
	}
	found := make([]translate.WebResult, len(results))
	for i, r := range results {
		found[i] = translate.WebResult{Title: r.Title, URL: r.URL, Snippet: r.Snippet, Age: r.Age}
	}
	return rewritten, &translate.WebSearch{Query: query, Results: found, Failure: failure}
}

// antigravitySearch searches through Google: it asks a Gemini model behind an
// Antigravity account a question with Google Search grounding, and reads the
// sources the answer was grounded in. It costs a call of a fast model, and needs
// no key beyond the account intact already holds.
type antigravitySearch struct{ a *api }

// searchModel answers the search. Not every model takes the grounding tool: the
// Gemini 3 ids answered 404 on this endpoint, and this one answered.
func searchModel() string {
	if m := strings.TrimSpace(os.Getenv("INTACT_SEARCH_MODEL")); m != "" {
		return m
	}
	return "gemini-2.5-flash"
}

func (s antigravitySearch) Search(ctx context.Context, query string, count int) ([]websearch.Result, error) {
	conns := s.a.activeConnections("antigravity")
	if len(conns) == 0 {
		return nil, fmt.Errorf("no active Antigravity account")
	}
	inner, _ := json.Marshal(map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "Search the web for: " + query +
			"\nReport what the sources say as short factual statements, with names, dates and numbers."}}}},
		"tools": []any{map[string]any{"googleSearch": map[string]any{}}},
	})
	var lastErr error
	for _, c := range conns {
		res, err := s.searchWith(ctx, c, inner, count)
		if err == nil {
			return res, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s antigravitySearch) searchWith(ctx context.Context, c store.Connection, inner []byte, count int) ([]websearch.Result, error) {
	p, ok := s.a.providerFor(c)
	if !ok {
		return nil, fmt.Errorf("antigravity account %s is not reachable", c.ID)
	}
	secret, err := s.a.secretFor(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	env, err := s.a.antigravityEnvelope(ctx, c, secret, searchModel(), inner)
	if err != nil {
		return nil, err
	}
	_, path := shapeFor(p, searchModel())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.a.send(req, p, c.Provider, path, secret, env)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("antigravity answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return readGrounding(resp.Body, count, followRedirects(ctx))
}

// readGrounding reads the stream of a grounded answer into results: each source
// the answer cites, with the statements it was cited for.
func readGrounding(body io.Reader, count int, resolve func(string) string) ([]websearch.Result, error) {
	type chunk struct {
		Web struct{ URI, Title string } `json:"web"`
	}
	var chunks []chunk
	var supports []struct {
		Segment struct{ Text string } `json:"segment"`
		Indices []int                 `json:"groundingChunkIndices"`
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data:")
		if line == sc.Text() {
			continue
		}
		var ev struct {
			Response struct {
				Candidates []struct {
					Grounding struct {
						Chunks   []chunk `json:"groundingChunks"`
						Supports []struct {
							Segment struct{ Text string } `json:"segment"`
							Indices []int                 `json:"groundingChunkIndices"`
						} `json:"groundingSupports"`
					} `json:"groundingMetadata"`
				} `json:"candidates"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		for _, cand := range ev.Response.Candidates {
			if len(cand.Grounding.Chunks) > 0 {
				chunks, supports = cand.Grounding.Chunks, cand.Grounding.Supports
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(chunks) > count && count > 0 {
		chunks = chunks[:count]
	}
	res := make([]websearch.Result, len(chunks))
	var wg sync.WaitGroup
	for i, c := range chunks {
		var said []string
		for _, sp := range supports {
			for _, idx := range sp.Indices {
				if idx == i && sp.Segment.Text != "" {
					said = append(said, sp.Segment.Text)
				}
			}
		}
		res[i] = websearch.Result{Title: c.Web.Title, URL: c.Web.URI, Snippet: strings.Join(said, " ")}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i].URL = resolve(res[i].URL)
		}(i)
	}
	wg.Wait()
	return res, nil
}

// followRedirects turns Google's grounding link into the page it stands for, so
// the client lists the real address. A link that does not resolve stays as it is.
func followRedirects(ctx context.Context) func(string) string {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(u string) string {
		if !strings.Contains(u, "grounding-api-redirect") {
			return u
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
		if err != nil {
			return u
		}
		resp, err := client.Do(req)
		if err != nil {
			return u
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); loc != "" && resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return loc
		}
		return u
	}
}

// droppedTools remembers which tools intact has said it drops, so the client
// is told each once and not on every request. There is no log to name them in:
// intact answers over HTTP and keeps no structured log of its own.
var droppedTools sync.Map

// serverTools answers a Messages request that declares tools Anthropic runs on
// its own servers (the MCP connector, tool search, web fetch), for a provider
// that is not Anthropic. intact runs them: it calls the model without a stream,
// runs the calls the model makes to those tools, and calls the model again with
// the results, until the model answers or asks the client for one of its own
// tools. The client gets the blocks Anthropic's API would have sent.
func (a *api) serverTools(w http.ResponseWriter, r *http.Request, body []byte, targets []store.Connection, start int, search *translate.WebSearch, cap *contract.Capture) {
	plan, err := servertools.Prepare(r.Context(), body)
	if err != nil {
		var re *servertools.RequestError
		if errors.As(err, &re) {
			writeError(w, http.StatusBadRequest, re.Message)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if search != nil {
		// A web search that ran before the first call: its blocks open the
		// answer, and the results stay in the request the model is sent.
		call, result := search.Blocks()
		plan.Preface(call, result, search.Requests())
	}
	req, err := plan.Request()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for {
		rec := &recorder{header: http.Header{}, code: http.StatusOK}
		a.failover(rec, r, req, targets, start, cap)
		if rec.code != http.StatusOK {
			rec.copyTo(w)
			return
		}
		next, final, err := plan.Advance(r.Context(), rec.body.Bytes())
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if next == nil {
			if m := rec.header.Get("X-Intact-Model"); m != "" {
				w.Header().Set("X-Intact-Model", m)
			}
			writeFinal(w, final, plan.Stream)
			return
		}
		req = next
	}
}

func writeFinal(w http.ResponseWriter, msg map[string]any, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		w.Write(servertools.MessageSSE(msg))
		return
	}
	b, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

// recorder is a ResponseWriter that keeps the answer, for the calls intact makes
// to a model on its own account.
type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Flush()              {}
func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.code, r.wrote = code, true
	}
}
func (r *recorder) Write(p []byte) (int, error) {
	r.wrote = true
	return r.body.Write(p)
}

// copyTo sends a recorded answer on as it came.
func (r *recorder) copyTo(w http.ResponseWriter) {
	for k, v := range r.header {
		w.Header()[k] = v
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(r.code)
	w.Write(r.body.Bytes())
}
