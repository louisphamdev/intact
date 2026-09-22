package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/upstream"
)

// Model rankings come from LMArena's public leaderboard (the
// lmarena-ai/leaderboard-dataset on Hugging Face, CC-BY-4.0): an Elo-style
// rating from people's votes between two anonymous answers. intact reads three
// boards once a day, keeps them in the database, and matches each provider's
// model ids to the board's names, so the model table can say how strong a
// model is.

// arenaBoards are the boards read: dataset config and category.
var arenaBoards = []struct{ Key, Config, Category string }{
	{"overall", "text", "overall"},
	{"coding", "text", "coding"},
	{"webdev", "webdev", "overall"},
}

const (
	arenaDataset   = "lmarena-ai/leaderboard-dataset"
	arenaRefresh   = 24 * time.Hour
	arenaSettingKy = "arena-ranking"
	arenaAliasKey  = "arena-aliases"
)

// arenaAPI is the Hugging Face datasets server. INTACT_ARENA_URL points it
// at a mirror of the same /rows API; tests set it directly.
var arenaAPI = func() string {
	if u := os.Getenv("INTACT_ARENA_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://datasets-server.huggingface.co"
}()

// ArenaScore is a model's place on one board.
type ArenaScore struct {
	Rating float64 `json:"rating"`
	Lower  float64 `json:"lower"`
	Upper  float64 `json:"upper"`
	Votes  int     `json:"votes"`
	Rank   int     `json:"rank"`
	// Tier is S, A, B, C or D by the gap to the board's top rating.
	Tier string `json:"tier"`
}

// ArenaModel is one leaderboard entry across boards.
type ArenaModel struct {
	Name   string                `json:"name"`
	Org    string                `json:"org,omitempty"`
	Scores map[string]ArenaScore `json:"scores"`
}

type arenaData struct {
	FetchedAt string       `json:"fetchedAt"`
	Published string       `json:"published"`
	Models    []ArenaModel `json:"models"`
}

// ArenaMatch is the leaderboard entry a provider model maps to.
type ArenaMatch struct {
	ArenaModel
	// How: exact (same name once normalised), variant (the name plus an
	// effort such as -high; the one with the most votes), alias (set by hand).
	How string `json:"how"`
}

type arenaState struct {
	mu    sync.Mutex
	data  arenaData
	index map[string][]int // normalised name → models
	busy  bool
}

// arenaTier grades a rating by its gap to the board's best: 100 points is
// about a 64% chance to win a vote.
func arenaTier(gap float64) string {
	switch {
	case gap <= 25:
		return "S"
	case gap <= 70:
		return "A"
	case gap <= 130:
		return "B"
	case gap <= 250:
		return "C"
	}
	return "D"
}

var (
	dateSuffix = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2}|\d{4})$`)
	dateInside = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})-`)
	// Suffixes that name a build or a packaging of a model, not another model.
	buildSuffixes = []string{"-latest", "-preview", "-exp", "-fp8-fast", "-fp8", "-fast", "-awq", "-int4",
		"-instruct", "-it", "-hf", "-versatile", "-001", "-002"}
	// Effort and mode suffixes a board lists a model under.
	effortSuffixes = map[string]bool{"max": true, "xhigh": true, "high": true, "medium": true, "low": true, "minimal": true,
		"thinking": true, "reasoning": true, "nothink": true, "non-thinking": true, "instant": true, "chat": true, "tiered": true}
)

// arenaNorm reduces a model id to a comparable name: no vendor path, no
// ":free" style tag, dots and underscores as dashes, no date or build suffix.
// "openai/gpt-oss-120b:free" and "@cf/openai/gpt-oss-120b" both give
// "gpt-oss-120b"; "claude-opus-4.6" and "claude-opus-4-6" meet.
func arenaNorm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.NewReplacer("_", "-", ".", "-", " ", "-").Replace(s)
	s = dateSuffix.ReplaceAllString(s, "")
	s = dateInside.ReplaceAllString(s, "-")
	for changed := true; changed; {
		changed = false
		for _, suf := range buildSuffixes {
			if strings.HasSuffix(s, suf) && len(s) > len(suf) {
				s, changed = strings.TrimSuffix(s, suf), true
			}
		}
	}
	return s
}

func (st *arenaState) load(d arenaData) {
	idx := map[string][]int{}
	for i, m := range d.Models {
		n := arenaNorm(m.Name)
		idx[n] = append(idx[n], i)
	}
	st.mu.Lock()
	st.data, st.index = d, idx
	st.mu.Unlock()
}

func votes(m ArenaModel) int {
	n := 0
	for _, s := range m.Scores {
		if s.Votes > n {
			n = s.Votes
		}
	}
	return n
}

// match finds the leaderboard entry of a model id: the same normalised name,
// else that name with an effort suffix (the most voted one).
func (st *arenaState) match(id string) (ArenaMatch, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := arenaNorm(id)
	best := func(is []int) ArenaModel {
		b := st.data.Models[is[0]]
		for _, i := range is[1:] {
			if votes(st.data.Models[i]) > votes(b) {
				b = st.data.Models[i]
			}
		}
		return b
	}
	if is, ok := st.index[n]; ok {
		return ArenaMatch{best(is), "exact"}, true
	}
	var cands []int
	for k, is := range st.index {
		if rest, ok := strings.CutPrefix(k, n+"-"); ok && effortSuffixes[rest] {
			cands = append(cands, is...)
		}
	}
	if len(cands) > 0 {
		sort.Ints(cands)
		return ArenaMatch{best(cands), "variant"}, true
	}
	return ArenaMatch{}, false
}

func (st *arenaState) byName(name string) (ArenaModel, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, m := range st.data.Models {
		if m.Name == name {
			return m, true
		}
	}
	return ArenaModel{}, false
}

// arenaAliases maps "provider/model" to a board name set by hand; "-" means
// the model has no entry.
func (a *api) arenaAliases() map[string]string {
	out := map[string]string{}
	if v, _ := a.store.GetSetting(arenaAliasKey); v != "" {
		json.Unmarshal([]byte(v), &out)
	}
	return out
}

// arenaFor maps a provider's models to their leaderboard entries.
func (a *api) arenaFor(prov string, models []string) map[string]ArenaMatch {
	aliases := a.arenaAliases()
	out := map[string]ArenaMatch{}
	for _, m := range models {
		if al, ok := aliases[prov+"/"+m]; ok {
			if al == "-" {
				continue
			}
			if am, ok := a.arena.byName(al); ok {
				out[m] = ArenaMatch{am, "alias"}
			}
			continue
		}
		if am, ok := a.arena.match(m); ok {
			out[m] = am
		}
	}
	return out
}

// fetchArena reads the boards from the datasets server, a page of 100 rows
// at a time.
func fetchArena(ctx context.Context) (arenaData, error) {
	type row struct {
		Name      string  `json:"model_name"`
		Org       string  `json:"organization"`
		Rating    float64 `json:"rating"`
		Lower     float64 `json:"rating_lower"`
		Upper     float64 `json:"rating_upper"`
		Votes     float64 `json:"vote_count"`
		Rank      float64 `json:"rank"`
		Published string  `json:"leaderboard_publish_date"`
	}
	// Read each config's rows in order with /rows, which the server caches
	// (its /filter is slow and fails often). Rows come grouped by category,
	// overall first, so reading stops once every wanted category was seen and
	// a page holds none of them.
	perConfig := map[string]map[string][]row{}
	for _, b := range arenaBoards {
		if perConfig[b.Config] == nil {
			perConfig[b.Config] = map[string][]row{}
		}
		perConfig[b.Config][b.Category] = nil
	}
	for cfg, want := range perConfig {
		for off := 0; off < 20000; off += 100 {
			q := url.Values{"dataset": {arenaDataset}, "config": {cfg}, "split": {"latest"},
				"offset": {fmt.Sprint(off)}, "length": {"100"}}
			var page struct {
				Rows []struct {
					Row struct {
						row
						Category string `json:"category"`
					} `json:"row"`
				} `json:"rows"`
				Total int `json:"num_rows_total"`
			}
			var err error
			for try := 0; try < 3; try++ {
				if err = getJSON(ctx, arenaAPI+"/rows?"+q.Encode(), &page); err == nil {
					break
				}
				time.Sleep(2 * time.Second)
			}
			if err != nil {
				return arenaData{}, fmt.Errorf("%s rows %d: %w", cfg, off, err)
			}
			hit := false
			for _, r := range page.Rows {
				if _, ok := want[r.Row.Category]; ok {
					want[r.Row.Category] = append(want[r.Row.Category], r.Row.row)
					hit = true
				}
			}
			all := true
			for _, rs := range want {
				all = all && len(rs) > 0
			}
			if (all && !hit) || off+100 >= page.Total {
				break
			}
		}
	}
	byName := map[string]*ArenaModel{}
	var order []string
	published := ""
	for _, b := range arenaBoards {
		rows := perConfig[b.Config][b.Category]
		if len(rows) == 0 {
			return arenaData{}, fmt.Errorf("%s/%s: empty board", b.Config, b.Category)
		}
		top := 0.0
		for _, r := range rows {
			if r.Rating > top {
				top = r.Rating
			}
			if r.Published > published {
				published = r.Published
			}
		}
		for _, r := range rows {
			m := byName[r.Name]
			if m == nil {
				m = &ArenaModel{Name: r.Name, Org: r.Org, Scores: map[string]ArenaScore{}}
				byName[r.Name] = m
				order = append(order, r.Name)
			}
			m.Scores[b.Key] = ArenaScore{Rating: round1(r.Rating), Lower: round1(r.Lower), Upper: round1(r.Upper),
				Votes: int(r.Votes), Rank: int(r.Rank), Tier: arenaTier(top - r.Rating)}
		}
	}
	d := arenaData{FetchedAt: time.Now().UTC().Format(time.RFC3339), Published: published}
	for _, n := range order {
		d.Models = append(d.Models, *byName[n])
	}
	return d, nil
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := upstream.Do(ctx, req, 1)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// refreshArena reads the boards again and stores them; one refresh at a time.
func (a *api) refreshArena(ctx context.Context) error {
	a.arena.mu.Lock()
	if a.arena.busy {
		a.arena.mu.Unlock()
		return errors.New("a refresh is running")
	}
	a.arena.busy = true
	a.arena.mu.Unlock()
	defer func() { a.arena.mu.Lock(); a.arena.busy = false; a.arena.mu.Unlock() }()
	d, err := fetchArena(ctx)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(d)
	if err := a.store.SetSetting(arenaSettingKy, string(b)); err != nil {
		return err
	}
	a.arena.load(d)
	return nil
}

// loadArena reads the stored boards at start.
func (a *api) loadArena() {
	if v, _ := a.store.GetSetting(arenaSettingKy); v != "" {
		var d arenaData
		if json.Unmarshal([]byte(v), &d) == nil {
			a.arena.load(d)
		}
	}
}

// arenaStale reports whether the boards are older than arenaRefresh.
func (a *api) arenaStale() bool {
	a.arena.mu.Lock()
	at := a.arena.data.FetchedAt
	a.arena.mu.Unlock()
	t, err := time.Parse(time.RFC3339, at)
	return err != nil || time.Since(t) > arenaRefresh
}

func (a *api) arenaMeta() map[string]any {
	a.arena.mu.Lock()
	defer a.arena.mu.Unlock()
	top := map[string]float64{}
	for _, m := range a.arena.data.Models {
		for b, s := range m.Scores {
			if s.Rating > top[b] {
				top[b] = s.Rating
			}
		}
	}
	return map[string]any{"fetchedAt": a.arena.data.FetchedAt, "published": a.arena.data.Published, "top": top,
		"models": len(a.arena.data.Models), "source": "LMArena (lmarena-ai/leaderboard-dataset, CC-BY-4.0)"}
}

// rankings serves the boards: GET /api/rankings (?q= filters names).
func (a *api) rankings(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	a.arena.mu.Lock()
	list := make([]ArenaModel, 0, len(a.arena.data.Models))
	for _, m := range a.arena.data.Models {
		if q == "" || strings.Contains(strings.ToLower(m.Name), q) {
			list = append(list, m)
		}
	}
	a.arena.mu.Unlock()
	out := a.arenaMeta()
	out["list"] = list
	writeJSON(w, out)
}

// refreshRankings reads the boards now: POST /api/rankings/refresh.
func (a *api) refreshRankings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := a.refreshArena(ctx); err != nil {
		writeError(w, http.StatusBadGateway, "cannot read the leaderboard: "+err.Error())
		return
	}
	writeJSON(w, a.arenaMeta())
}

// setArenaAlias maps a model by hand: {"provider","model","name"}; name ""
// returns to automatic matching, "-" marks the model as not on the board.
func (a *api) setArenaAlias(w http.ResponseWriter, r *http.Request) {
	var b struct{ Provider, Model, Name string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&b); err != nil || b.Provider == "" || b.Model == "" {
		writeError(w, http.StatusBadRequest, "provider and model are required")
		return
	}
	if b.Name != "" && b.Name != "-" {
		if _, ok := a.arena.byName(b.Name); !ok {
			writeError(w, http.StatusBadRequest, "no leaderboard entry is named "+b.Name)
			return
		}
	}
	al := a.arenaAliases()
	if b.Name == "" {
		delete(al, b.Provider+"/"+b.Model)
	} else {
		al[b.Provider+"/"+b.Model] = b.Name
	}
	raw, _ := json.Marshal(al)
	if err := a.store.SetSetting(arenaAliasKey, string(raw)); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot save")
		return
	}
	writeJSON(w, map[string]any{"match": a.arenaFor(b.Provider, []string{b.Model})[b.Model]})
}

// arenaLoop keeps the boards at most a day old.
func (a *api) arenaLoop() {
	time.Sleep(2 * time.Minute)
	for {
		if a.arenaStale() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			if err := a.refreshArena(ctx); err != nil {
				log.Printf("rankings: %v", err)
			}
			cancel()
		}
		time.Sleep(time.Hour)
	}
}
