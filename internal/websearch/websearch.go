// Package websearch runs the web searches that a client asks its model for.
//
// Claude Code's WebSearch tool does not search: it sends a request that declares
// Anthropic's hosted tool (type web_search_20250305) and lets Anthropic's servers
// run the search and answer. No other provider has that tool, so a model behind
// intact would answer from memory and name sources it never read. intact runs the
// search itself, gives the model the results, and hands the client the blocks
// Anthropic would have (see internal/translate, Reply.Search).
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Result is one page a search found.
type Result struct {
	Title   string
	URL     string
	Snippet string
	// Age says how old the page is, as the search service words it. It may be empty.
	Age string
}

// Searcher answers a query with up to count results.
type Searcher interface {
	Search(ctx context.Context, query string, count int) ([]Result, error)
}

// Providers are the search services intact can call, each with its own key.
var Providers = []string{"brave", "tavily", "serper", "exa"}

// Config says which service to call and how.
type Config struct {
	Provider string
	Key      string
	// BaseURL replaces the service's address, for a test or a proxy.
	BaseURL string
	Count   int
	Timeout time.Duration
	Client  *http.Client
}

// FromEnv reads the search settings from the environment: INTACT_SEARCH_PROVIDER
// (one of Providers), INTACT_SEARCH_KEY, and optionally INTACT_SEARCH_COUNT and
// INTACT_SEARCH_URL. It returns nil when no provider is set: web search is then
// not configured, and a client that asks for it is told so.
func FromEnv() *Config {
	p := strings.ToLower(strings.TrimSpace(os.Getenv("INTACT_SEARCH_PROVIDER")))
	if p == "" {
		return nil
	}
	c := &Config{Provider: p, Key: strings.TrimSpace(os.Getenv("INTACT_SEARCH_KEY")), BaseURL: strings.TrimRight(os.Getenv("INTACT_SEARCH_URL"), "/")}
	if n, err := strconv.Atoi(os.Getenv("INTACT_SEARCH_COUNT")); err == nil {
		c.Count = n
	}
	return c
}

// New builds the searcher a config names.
func New(c Config) (Searcher, error) {
	c.Count = c.EffectiveCount()
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Second
	}
	if c.Client == nil {
		c.Client = http.DefaultClient
	}
	if c.Key == "" {
		return nil, fmt.Errorf("the %s search needs a key (INTACT_SEARCH_KEY)", c.Provider)
	}
	switch c.Provider {
	case "brave", "tavily", "serper", "exa":
		return &service{c: c}, nil
	}
	return nil, fmt.Errorf("unknown search provider %q; use one of %s", c.Provider, strings.Join(Providers, ", "))
}

// DefaultCount is how many results a search asks for when Count is unset or
// out of range.
const DefaultCount = 5

// EffectiveCount is how many results one search asks for: Count when it is
// between 1 and 20, DefaultCount otherwise.
func (c Config) EffectiveCount() int {
	if c.Count <= 0 || c.Count > 20 {
		return DefaultCount
	}
	return c.Count
}

type service struct{ c Config }

func (s *service) Search(ctx context.Context, query string, count int) ([]Result, error) {
	if count <= 0 {
		count = s.c.EffectiveCount()
	}
	ctx, cancel := context.WithTimeout(ctx, s.c.Timeout)
	defer cancel()
	switch s.c.Provider {
	case "brave":
		return s.brave(ctx, query, count)
	case "tavily":
		return s.tavily(ctx, query, count)
	case "serper":
		return s.serper(ctx, query, count)
	}
	return s.exa(ctx, query, count)
}

func (s *service) base(def string) string {
	if s.c.BaseURL != "" {
		return s.c.BaseURL
	}
	return def
}

// do sends one request and decodes a JSON answer into out.
func (s *service) do(req *http.Request, out any) error {
	resp, err := s.c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("%s answered %d: %s", s.c.Provider, resp.StatusCode, msg)
	}
	return json.Unmarshal(raw, out)
}

func (s *service) post(ctx context.Context, url string, header map[string]string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return s.do(req, out)
}

func (s *service) brave(ctx context.Context, query string, count int) ([]Result, error) {
	u := s.base("https://api.search.brave.com/res/v1/web/search") + "?q=" + url.QueryEscape(query) + "&count=" + strconv.Itoa(count)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Subscription-Token", s.c.Key)
	req.Header.Set("Accept", "application/json")
	var out struct {
		Web struct {
			Results []struct{ Title, URL, Description, Age string } `json:"results"`
		} `json:"web"`
	}
	if err := s.do(req, &out); err != nil {
		return nil, err
	}
	var res []Result
	for _, r := range out.Web.Results {
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: r.Description, Age: r.Age})
	}
	return res, nil
}

func (s *service) tavily(ctx context.Context, query string, count int) ([]Result, error) {
	var out struct {
		Results []struct{ Title, URL, Content string } `json:"results"`
	}
	err := s.post(ctx, s.base("https://api.tavily.com/search"), map[string]string{"Authorization": "Bearer " + s.c.Key},
		map[string]any{"query": query, "max_results": count}, &out)
	if err != nil {
		return nil, err
	}
	var res []Result
	for _, r := range out.Results {
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return res, nil
}

func (s *service) serper(ctx context.Context, query string, count int) ([]Result, error) {
	var out struct {
		Organic []struct{ Title, Link, Snippet, Date string } `json:"organic"`
	}
	err := s.post(ctx, s.base("https://google.serper.dev/search"), map[string]string{"X-API-KEY": s.c.Key},
		map[string]any{"q": query, "num": count}, &out)
	if err != nil {
		return nil, err
	}
	var res []Result
	for _, r := range out.Organic {
		res = append(res, Result{Title: r.Title, URL: r.Link, Snippet: r.Snippet, Age: r.Date})
	}
	return res, nil
}

func (s *service) exa(ctx context.Context, query string, count int) ([]Result, error) {
	var out struct {
		Results []struct {
			Title         string
			URL           string
			Text          string
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	err := s.post(ctx, s.base("https://api.exa.ai/search"), map[string]string{"x-api-key": s.c.Key},
		map[string]any{"query": query, "numResults": count, "contents": map[string]any{"text": map[string]any{"maxCharacters": 500}}}, &out)
	if err != nil {
		return nil, err
	}
	var res []Result
	for _, r := range out.Results {
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: r.Text, Age: r.PublishedDate})
	}
	return res, nil
}
