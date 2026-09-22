package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// Drift review: a decision model judges each recorded change, so the ones a
// client or its data caused are acknowledged without a person.
//
// intact computes what it knows of the change (where the path lies, whether
// the same field came and went today, whether a new client started streaming
// or sending tools that hour, how many answers failed since) and asks
// TypeSafe's Jev, a model built for calibrated decisions, to choose a cause.
// A benign cause with confidence of at least reviewAckConfidence, and no
// failed answer since, acknowledges the change; anything else waits for a
// person with the verdict beside it.
//
// Chosen on 2026-09-22 after a trial on 48 real changes: with these facts Jev
// matched a hand review on 44, and the 4 others were split between two
// benign causes. Asked yes/no questions instead ("blacklist it?", "does a
// person need to see it?"), it answered 0.15–0.75 and told nothing apart, so
// only the cause is asked.

const (
	reviewModel         = "typesafe/jev-latest"
	reviewAckConfidence = 0.6
	reviewEvery         = time.Minute
	reviewBatch         = 20
	reviewBackoff       = 10 * time.Minute
	reviewSettingKey    = "drift-review"
)

// Causes the reviewer chooses between. The first three are benign.
const (
	CauseNewClient      = "new_client_usage"
	CauseDataNoise      = "data_noise"
	CauseOptionalFlap   = "optional_field_flap"
	CauseProviderChange = "provider_format_change"
)

func benignCause(c string) bool {
	return c == CauseNewClient || c == CauseDataNoise || c == CauseOptionalFlap
}

var reviewQuestions = map[string]any{
	"cause": map[string]any{"type": "choice", "instructions": "What most likely caused this recorded structure change?",
		"criteria": map[string]string{
			CauseNewClient:      "A client started using a feature or a new client began sending traffic (streaming, tools, tool calls, reasoning options, JSON schema keywords in tool definitions); the provider's API itself did not change",
			CauseProviderChange: "The provider changed the structure of its answer, or began requiring or rejecting something it did not before",
			CauseDataNoise:      "The path lies inside user data or tool call arguments (values chosen by a tool or a user), not a field of the API",
			CauseOptionalFlap:   "An optional field that some requests or answers carry and others do not, reported as removed then returned",
		}},
}

// errTrack counts the failed answers of each provider, for the reviewer.
type errTrack struct {
	mu sync.Mutex
	m  map[string][]time.Time
}

func (e *errTrack) note(prov string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	list := e.m[prov]
	// Keep a day.
	for len(list) > 0 && now.Sub(list[0]) > 24*time.Hour {
		list = list[1:]
	}
	e.m[prov] = append(list, now)
}

func (e *errTrack) since(prov string, t time.Time) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, at := range e.m[prov] {
		if !at.Before(t) {
			n++
		}
	}
	return n
}

type reviewState struct {
	mu        sync.Mutex
	pauseTill time.Time
	lastError string
}

func (a *api) reviewEnabled() bool {
	v, _ := a.store.GetSetting(reviewSettingKey)
	return v != "off"
}

// reviewFacts is what intact itself knows of a change.
func (a *api) reviewFacts(c store.ShapeChange, around []store.ShapeChange) map[string]any {
	at, _ := time.Parse(time.RFC3339, c.At)
	sameHour := func(x store.ShapeChange) bool {
		t, err := time.Parse(time.RFC3339, x.At)
		return err == nil && t.Sub(at) < time.Hour && at.Sub(t) < time.Hour
	}
	burst, newClient := 0, false
	kinds := map[string]bool{}
	clientFirst := c.At
	for _, x := range around {
		if x.Provider != c.Provider {
			continue
		}
		if sameHour(x) {
			burst++
			if x.Direction == "request" && x.Kind == "added" && (x.Path == "stream" || x.Path == "tools" || x.Path == "messages[].tool_calls") {
				newClient = true
			}
		}
		if x.Path == c.Path && x.Direction == c.Direction {
			kinds[x.Kind] = true
		}
		if c.Client != "" && x.Client == c.Client && x.At < clientFirst {
			clientFirst = x.At
		}
	}
	first, _ := time.Parse(time.RFC3339, clientFirst)
	p := c.Path
	return map[string]any{
		"inside_tool_call_arguments":                    strings.Contains(p, ".args") || strings.Contains(p, ".arguments") || strings.Contains(p, ".input."),
		"inside_tool_definition_schema":                 strings.Contains(p, "tools[]") && strings.Contains(p, "parameters"),
		"same_field_removed_and_returned_today":         kinds["removed"] && kinds["returned"] || c.Kind == "returned" && kinds["removed"],
		"streamed_answer":                               c.Direction == "response" && (c.Event != "" || strings.Contains(p, "delta") || strings.Contains(c.Endpoint, "stream")),
		"a_client_started_streaming_or_tools_this_hour": newClient,
		"client":                                firstNonEmpty(c.Client, "unknown"),
		"client_first_seen_within_an_hour":      c.Client != "" && at.Sub(first) < time.Hour,
		"other_changes_same_provider_same_hour": burst - 1,
		"failed_answers_since":                  a.errs.since(c.Provider, at),
	}
}

// askJev sends one System One request through intact's own /v1, as a client
// would, and returns the answers.
func (a *api) askJev(ctx context.Context, state any, questions map[string]any) (map[string]typeSafeAnswer, error) {
	st, _ := json.Marshal(state)
	body, _ := json.Marshal(map[string]any{"model": reviewModel, "state": string(st), "questions": questions})
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/systemone", strings.NewReader(string(body)))
	req.SetPathValue("path", "systemone")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.v1(rec, req)
	raw, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("%s: %d %s", reviewModel, rec.Code, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
	}
	var d struct {
		Answers map[string]typeSafeAnswer `json:"answers"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Answers == nil {
		return nil, errors.New("the decision model gave no answers")
	}
	return d.Answers, nil
}

// reviewChange judges one change and records the verdict.
func (a *api) reviewChange(ctx context.Context, c store.ShapeChange, around []store.ShapeChange) (string, float64, bool, error) {
	facts := a.reviewFacts(c, around)
	state := map[string]any{
		"what":     "intact records when the JSON structure passing through it changes. direction=request is what a client sent to intact; direction=response is what the provider answered.",
		"provider": c.Provider, "direction": c.Direction, "endpoint": c.Endpoint, "event": c.Event, "path": c.Path,
		"change": c.Kind, "old_type": c.OldType, "new_type": c.NewType, "facts": facts,
		"sample_start": c.Sample[:min(len(c.Sample), 200)],
	}
	ans, err := a.askJev(ctx, state, reviewQuestions)
	if err != nil {
		return "", 0, false, err
	}
	cause := ans["cause"]
	conf := 0.0
	if cause.Confidence != nil {
		conf = *cause.Confidence
	}
	if cause.Choice == "" {
		return "", 0, false, errors.New("the decision model chose no cause")
	}
	ack := benignCause(cause.Choice) && conf >= reviewAckConfidence && facts["failed_answers_since"].(int) == 0
	if err := a.store.SetShapeVerdict(c.ID, cause.Choice, conf, ack); err != nil {
		return "", 0, false, err
	}
	return cause.Choice, conf, ack, nil
}

// reviewPending judges the changes waiting for a verdict; it stops at the
// first failure of the decision model and pauses the reviews for a while.
func (a *api) reviewPending(ctx context.Context) (int, error) {
	if !a.reviewEnabled() {
		return 0, nil
	}
	if len(a.activeConnections("typesafe")) == 0 {
		return 0, errors.New("no active TypeSafe account for the decision model")
	}
	pending, err := a.store.UnreviewedShapeChanges(reviewBatch)
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	around, _ := a.store.ShapeChangesSince(time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339))
	n := 0
	for _, c := range pending {
		if _, _, _, err := a.reviewChange(ctx, c, around); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// reviewLoop runs the reviews in the background.
func (a *api) reviewLoop() {
	time.Sleep(90 * time.Second)
	for {
		a.review.mu.Lock()
		paused := time.Now().Before(a.review.pauseTill)
		a.review.mu.Unlock()
		if !paused {
			a.drift.Drain()
			a.drift.Flush()
			if n, err := a.reviewPending(context.Background()); err != nil {
				log.Printf("drift review: %v", err)
				a.review.mu.Lock()
				a.review.pauseTill, a.review.lastError = time.Now().Add(reviewBackoff), err.Error()
				a.review.mu.Unlock()
			} else if n > 0 {
				a.review.mu.Lock()
				a.review.lastError = ""
				a.review.mu.Unlock()
				log.Printf("drift review: judged %d change(s)", n)
			}
		}
		time.Sleep(reviewEvery)
	}
}

// driftReview serves the review's state; POST {"enabled":bool} switches it,
// POST {"run":true} judges the pending changes now.
func (a *api) driftReview(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var b struct {
			Enabled *bool `json:"enabled"`
			Run     bool  `json:"run"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
			writeError(w, http.StatusBadRequest, "bad json")
			return
		}
		if b.Enabled != nil {
			v := "on"
			if !*b.Enabled {
				v = "off"
			}
			a.store.SetSetting(reviewSettingKey, v)
		}
		if b.Run {
			n, err := a.reviewPending(r.Context())
			if err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			a.review.mu.Lock()
			a.review.pauseTill, a.review.lastError = time.Time{}, ""
			a.review.mu.Unlock()
			writeJSON(w, map[string]any{"judged": n, "enabled": a.reviewEnabled()})
			return
		}
	}
	a.review.mu.Lock()
	lastErr := a.review.lastError
	a.review.mu.Unlock()
	writeJSON(w, map[string]any{"enabled": a.reviewEnabled(), "model": reviewModel, "ackConfidence": reviewAckConfidence,
		"ready": len(a.activeConnections("typesafe")) > 0, "lastError": lastErr})
}
