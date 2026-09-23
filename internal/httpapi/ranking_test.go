package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

func TestArenaNorm(t *testing.T) {
	for in, want := range map[string]string{
		"openai/gpt-oss-120b:free":           "gpt-oss-120b",
		"@cf/openai/gpt-oss-120b":            "gpt-oss-120b",
		"claude-opus-4.6":                    "claude-opus-4-6",
		"claude-haiku-4-5-20251001":          "claude-haiku-4-5",
		"@cf/meta/llama-3.1-8b-instruct-fp8": "llama-3-1-8b",
		"llama-3.1-8b-instruct":              "llama-3-1-8b",
		"meta-llama/llama-3.3-70b-versatile": "llama-3-3-70b",
		"gpt-4o-2024-11-20":                  "gpt-4o",
		"qwen/qwen3.8-max-0902":              "qwen3-8-max",
	} {
		if got := arenaNorm(in); got != want {
			t.Errorf("arenaNorm(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeArena serves two boards of the datasets server's /filter shape.
func fakeArena(t *testing.T, boards map[string][][3]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// All rows of the config, grouped by category (overall first), paged.
		q := r.URL.Query()
		var all []map[string]any
		for _, cat := range []string{"overall", "coding", "math"} {
			for i, b := range boards[q.Get("config")+"/"+cat] {
				all = append(all, map[string]any{"row": map[string]any{"model_name": b[0], "rating": b[1], "vote_count": b[2], "category": cat,
					"rank": i + 1, "rating_lower": 0, "rating_upper": 0, "organization": "x", "leaderboard_publish_date": "2026-09-13"}})
			}
		}
		off, _ := strconv.Atoi(q.Get("offset"))
		n, _ := strconv.Atoi(q.Get("length"))
		page := []map[string]any{}
		for i := off; i < off+n && i < len(all); i++ {
			page = append(page, all[i])
		}
		json.NewEncoder(w).Encode(map[string]any{"rows": page, "num_rows_total": len(all)})
	}))
}

func TestArenaMatchesProviderModels(t *testing.T) {
	up := fakeArena(t, map[string][][3]any{
		"text/overall": {{"claude-opus-5-high", 1505.0, 40000.0}, {"claude-opus-5-max", 1506.0, 9000.0}, {"gpt-oss-120b", 1380.0, 30000.0},
			{"llama-3.1-8b-instruct", 1180.0, 50000.0}, {"gemini-3.8-flash-high", 1494.0, 5000.0}},
		"text/coding":    {{"claude-opus-5-high", 1530.0, 20000.0}, {"gpt-oss-120b", 1400.0, 10000.0}},
		"text/math":      {{"gpt-oss-120b", 9999.0, 1.0}},
		"webdev/overall": {{"claude-opus-5-high", 1600.0, 3000.0}},
	})
	defer up.Close()
	old := arenaAPI
	arenaAPI = up.URL
	defer func() { arenaAPI = old }()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	a := &api{store: s}
	if err := a.refreshArena(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := a.arenaFor("x", []string{"claude-opus-5", "openai/gpt-oss-120b:free", "@cf/meta/llama-3.1-8b-instruct-fp8", "gemini-3.8-flash", "whisper-large-v3"})
	want := map[string]string{"claude-opus-5": "claude-opus-5-high variant", "openai/gpt-oss-120b:free": "gpt-oss-120b exact",
		"@cf/meta/llama-3.1-8b-instruct-fp8": "llama-3.1-8b-instruct exact", "gemini-3.8-flash": "gemini-3.8-flash-high variant"}
	for m, w := range want {
		if g := got[m]; g.Name+" "+g.How != w {
			t.Errorf("%s -> %q, want %q", m, g.Name+" "+g.How, w)
		}
	}
	if _, ok := got["whisper-large-v3"]; ok {
		t.Error("whisper matched")
	}
	opus := got["claude-opus-5"]
	if opus.Scores["overall"].Tier != "S" || opus.Scores["coding"].Rating != 1530 || opus.Scores["webdev"].Rank != 1 {
		t.Errorf("opus scores = %+v", opus.Scores)
	}
	if got["@cf/meta/llama-3.1-8b-instruct-fp8"].Scores["overall"].Tier != "D" || got["openai/gpt-oss-120b:free"].Scores["overall"].Tier != "B" {
		t.Errorf("tiers: llama %s, oss %s", got["@cf/meta/llama-3.1-8b-instruct-fp8"].Scores["overall"].Tier, got["openai/gpt-oss-120b:free"].Scores["overall"].Tier)
	}
	// Stored: a new api reads the same boards without fetching.
	b := &api{store: s}
	b.loadArena()
	if len(b.arenaFor("x", []string{"claude-opus-5"})) != 1 {
		t.Error("boards not kept")
	}
}

func TestArenaAlias(t *testing.T) {
	up := fakeArena(t, map[string][][3]any{
		"text/overall": {{"claude-opus-5-high", 1505.0, 40000.0}, {"kimi-k3-max", 1450.0, 1000.0}}, "text/coding": {{"kimi-k3-max", 1400.0, 10.0}}, "webdev/overall": {{"kimi-k3-max", 1400.0, 10.0}},
	})
	defer up.Close()
	old := arenaAPI
	arenaAPI = up.URL
	defer func() { arenaAPI = old }()
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest("POST", "/rankings/alias", strings.NewReader(body)))
		return rec
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("POST", "/rankings/refresh", nil))
	if rec.Code != 200 {
		t.Fatalf("refresh %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"provider":"github","model":"kimi-k3-copilot","name":"kimi-k3-max"}`); !strings.Contains(rec.Body.String(), `"how":"alias"`) {
		t.Errorf("alias: %s", rec.Body.String())
	}
	if rec := post(`{"provider":"github","model":"x","name":"no-such"}`); rec.Code != 400 {
		t.Errorf("unknown name: %d", rec.Code)
	}
	post(`{"provider":"github","model":"claude-opus-5","name":"-"}`)
	s.SyncModels("github", []string{"kimi-k3-copilot", "claude-opus-5"}, false, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, loopbackRequest("GET", "/providers/github/model-table", nil))
	var d struct{ Arena map[string]ArenaMatch }
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Arena["kimi-k3-copilot"].Name != "kimi-k3-max" || fmt.Sprint(d.Arena["claude-opus-5"].Name) != "" {
		t.Errorf("table arena = %+v", d.Arena)
	}
}
