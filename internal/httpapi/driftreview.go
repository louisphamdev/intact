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

	"github.com/louisphamdev/intact/internal/provider"
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
	reviewEvery      = time.Minute
	reviewBatch      = 20
	reviewBackoff    = 10 * time.Minute
	reviewSettingKey = "drift-review" // before the config: "off" or not
	reviewConfigKey  = "drift-review-config"
	defaultAckConf   = 0.6
)

// ReviewConfig is the drift review's settings, changed from the dashboard,
// the API or MCP.
//   - DecisionModel: a System One model (a TypeSafe-shaped provider) that
//     chooses each change's cause. Without one, the review cannot be on.
//   - ResolverModel: any chat model intact serves. It takes the changes the
//     decision model was not sure of; empty leaves them to a person.
//   - AckConfidence: the decision model's confidence needed to act alone.
type ReviewConfig struct {
	Enabled       bool    `json:"enabled"`
	DecisionModel string  `json:"decisionModel"`
	ResolverModel string  `json:"resolverModel"`
	AckConfidence float64 `json:"ackConfidence"`
}

func (a *api) reviewConfig() ReviewConfig {
	var c ReviewConfig
	if v, _ := a.store.GetSetting(reviewConfigKey); v != "" {
		json.Unmarshal([]byte(v), &c)
	} else if old, _ := a.store.GetSetting(reviewSettingKey); old != "off" && len(a.activeConnections("typesafe")) > 0 {
		// The review of the first version: on, with Jev, and nothing else.
		c = ReviewConfig{Enabled: true, DecisionModel: "typesafe/jev-latest"}
	}
	if c.AckConfidence <= 0 {
		c.AckConfidence = defaultAckConf
	}
	if c.DecisionModel == "" {
		c.Enabled = false
	}
	return c
}

// checkReviewConfig refuses a model intact cannot use in that role.
func (a *api) checkReviewConfig(c ReviewConfig) error {
	if c.DecisionModel != "" {
		prov, _ := a.splitModel(c.DecisionModel)
		p, ok := provider.Lookup(prov)
		if prov == "" || !ok || p.API != "typesafe" {
			return errors.New("decisionModel: a System One model, <provider>/<model> of a TypeSafe provider (such as typesafe/jev-latest)")
		}
	}
	if c.ResolverModel != "" {
		prov, _ := a.splitModel(c.ResolverModel)
		p, ok := provider.Lookup(prov)
		if prov == "" || !ok || p.API == "typesafe" {
			return errors.New("resolverModel: a chat model intact serves, <provider>/<model> (such as antigravity/gemini-3.8-flash)")
		}
	}
	if c.Enabled && c.DecisionModel == "" {
		return errors.New("the review needs a decision model before it can be on")
	}
	if c.AckConfidence < 0.5 || c.AckConfidence > 0.99 {
		return errors.New("ackConfidence: 0.5 to 0.99")
	}
	return nil
}

func (a *api) reviewReady(c ReviewConfig) bool {
	prov, _ := a.splitModel(c.DecisionModel)
	return prov != "" && len(a.activeConnections(prov)) > 0
}

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
func (a *api) askJev(ctx context.Context, model string, state any, questions map[string]any) (map[string]typeSafeAnswer, error) {
	st, _ := json.Marshal(state)
	body, _ := json.Marshal(map[string]any{"model": model, "state": string(st), "questions": questions})
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/systemone", strings.NewReader(string(body)))
	req.SetPathValue("path", "systemone")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.v1(rec, req)
	raw, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("%s: %d %s", model, rec.Code, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
	}
	var d struct {
		Answers map[string]typeSafeAnswer `json:"answers"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Answers == nil {
		return nil, errors.New("the decision model gave no answers")
	}
	return d.Answers, nil
}

// reviewState describes a change for the models.
func reviewDescription(c store.ShapeChange, facts map[string]any) map[string]any {
	return map[string]any{
		"what":     "intact records when the JSON structure passing through it changes. direction=request is what a client sent to intact; direction=response is what the provider answered.",
		"provider": c.Provider, "direction": c.Direction, "endpoint": c.Endpoint, "event": c.Event, "path": c.Path,
		"change": c.Kind, "old_type": c.OldType, "new_type": c.NewType, "facts": facts,
		"sample_start": c.Sample[:min(len(c.Sample), 200)],
	}
}

// reviewChange judges one change and records the verdict: the decision
// model's when it is sure, the resolver's when it is not and one is set.
func (a *api) reviewChange(ctx context.Context, cfg ReviewConfig, c store.ShapeChange, around []store.ShapeChange) (store.Verdict, error) {
	facts := a.reviewFacts(c, around)
	ans, err := a.askJev(ctx, cfg.DecisionModel, reviewDescription(c, facts), reviewQuestions)
	if err != nil {
		return store.Verdict{}, err
	}
	cause := ans["cause"]
	conf := 0.0
	if cause.Confidence != nil {
		conf = *cause.Confidence
	}
	if cause.Choice == "" {
		return store.Verdict{}, errors.New("the decision model chose no cause")
	}
	failed := facts["failed_answers_since"].(int) > 0
	v := store.Verdict{Cause: cause.Choice, Conf: conf, By: cfg.DecisionModel,
		Ack: benignCause(cause.Choice) && conf >= cfg.AckConfidence && !failed}
	switch {
	case v.Ack:
	case cfg.ResolverModel != "":
		// A resolver settles everything the decision model did not: nothing
		// waits for a person.
		return a.resolveChange(ctx, cfg, c, facts, cause)
	case failed:
		v.Note = "answers failed since the change; no resolver is set"
	case conf < cfg.AckConfidence:
		v.Note = "the decision model was not sure; no resolver is set"
	default:
		v.Note = "a likely provider change; no resolver is set"
	}
	return v, a.store.SetShapeVerdict(c.ID, v)
}

// resolverPrompt tells a chat model how to settle a change the decision
// model was unsure of.
const resolverPrompt = `You review structure changes that intact, an LLM gateway, records in the JSON passing through it. direction=request is what a client sent; direction=response is what the provider answered. A decision model already looked at this change but was not sure.

Choose the cause:
- new_client_usage: a client started using a feature or a new client began (streaming, tools, tool calls, reasoning options, JSON schema keywords); the provider's API did not change.
- data_noise: the path lies in user data or tool call arguments, not a field of the API.
- optional_field_flap: an optional field some documents carry and others do not.
- provider_format_change: the provider changed its answer, or began requiring or rejecting something.

No person will look after you: your action is final. Choose it:
- acknowledge: record the change as reviewed. Right for every harmless change, and for a provider's new answer field (intact passes answers through whole, so nothing breaks).
- blacklist: strip this request field before it reaches this provider. Only for a request field that the provider rejects, shown by answers failing since the change. Never for a field the client needs (model, messages, tools, stream).

Answer with one JSON object and nothing else: {"cause":"<one of the four>","action":"acknowledge"|"blacklist","reason":"<one short sentence>"}`

// resolveChange asks the resolver model to settle a change.
func (a *api) resolveChange(ctx context.Context, cfg ReviewConfig, c store.ShapeChange, facts map[string]any, jev typeSafeAnswer) (store.Verdict, error) {
	jevConf := 0.0
	if jev.Confidence != nil {
		jevConf = *jev.Confidence
	}
	desc := reviewDescription(c, facts)
	desc["decision_model"] = map[string]any{"leaning": jev.Choice, "confidence": jevConf, "probabilities": jev.Probabilities}
	in, _ := json.Marshal(desc)
	body, _ := json.Marshal(map[string]any{"model": cfg.ResolverModel, "max_tokens": 1024,
		"messages": []any{map[string]any{"role": "system", "content": resolverPrompt}, map[string]any{"role": "user", "content": string(in)}}})
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.SetPathValue("path", "chat/completions")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.v1(rec, req)
	raw, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK {
		return store.Verdict{}, fmt.Errorf("%s: %d %s", cfg.ResolverModel, rec.Code, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
	}
	var d struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(raw, &d)
	if len(d.Choices) == 0 {
		return store.Verdict{}, fmt.Errorf("%s gave no answer", cfg.ResolverModel)
	}
	text := d.Choices[0].Message.Content
	var out struct {
		Cause  string `json:"cause"`
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j <= i || json.Unmarshal([]byte(text[i:j+1]), &out) != nil || out.Cause == "" {
		return store.Verdict{}, fmt.Errorf("%s answered no verdict: %.120s", cfg.ResolverModel, text)
	}
	// The resolver's action is final: the change is closed either way.
	v := store.Verdict{Cause: out.Cause, Conf: jevConf, By: cfg.ResolverModel, Note: out.Reason, Resolved: true, Ack: true}
	if out.Action == "blacklist" {
		if why := a.autoBlacklist(c, facts); why != "" {
			v.Note = strings.TrimSpace(v.Note + " (" + why + ")")
		}
	}
	return v, a.store.SetShapeVerdict(c.ID, v)
}

// protectedFields are never stripped on a model's word: without them the
// request means something else.
var protectedFields = map[string]bool{"model": true, "messages": true, "input": true, "tools": true, "stream": true,
	"system": true, "max_tokens": true, "contents": true, "prompt": true}

// autoBlacklist adds a field rule for a request field a provider refuses, when
// the evidence holds: a request field that appeared or changed type, answers
// failing since, and not a field the request needs. It says what it did.
func (a *api) autoBlacklist(c store.ShapeChange, facts map[string]any) string {
	top := c.Path
	if i := strings.IndexAny(top, ".["); i >= 0 {
		top = top[:i]
	}
	switch {
	case c.Direction != "request" || (c.Kind != "added" && c.Kind != "type"):
		return "not blacklisted: only a new request field can be"
	case facts["failed_answers_since"].(int) == 0:
		return "not blacklisted: no answer failed since"
	case protectedFields[top]:
		return "not blacklisted: the request needs " + top
	}
	pattern := toFieldPattern(c.Path)
	if _, err := a.putFilter(store.Filter{Provider: c.Provider, Kind: "field", Pattern: pattern,
		Note: "drift review: " + c.Kind + " on " + c.Endpoint, Enabled: true}); err != nil {
		return "blacklist failed: " + err.Error()
	}
	return "blacklisted " + pattern
}

// reviewPending judges the changes waiting for a verdict, then hands the
// ones judged before a resolver was set to it. It stops at the first failure
// of a model.
func (a *api) reviewPending(ctx context.Context) (int, error) {
	cfg := a.reviewConfig()
	if !cfg.Enabled {
		return 0, nil
	}
	if !a.reviewReady(cfg) {
		return 0, fmt.Errorf("no active account serves the decision model %s", cfg.DecisionModel)
	}
	pending, err := a.store.UnreviewedShapeChanges(reviewBatch)
	if err != nil {
		return 0, err
	}
	var unsure []store.ShapeChange
	if cfg.ResolverModel != "" {
		unsure, _ = a.store.UnresolvedShapeChanges(reviewBatch)
	}
	if len(pending) == 0 && len(unsure) == 0 {
		return 0, nil
	}
	around, _ := a.store.ShapeChangesSince(time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339))
	n := 0
	for _, c := range pending {
		if _, err := a.reviewChange(ctx, cfg, c, around); err != nil {
			return n, err
		}
		n++
	}
	for _, c := range unsure {
		conf := c.VerdictConf
		jev := typeSafeAnswer{Choice: c.Verdict, Confidence: &conf}
		if _, err := a.resolveChange(ctx, cfg, c, a.reviewFacts(c, around), jev); err != nil {
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

// driftReview serves the review's config and state. POST takes config
// fields ({"enabled","decisionModel","resolverModel","ackConfidence"}; those
// left out keep their value) and {"run":true} to judge the waiting changes now.
func (a *api) driftReview(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var b struct {
			Enabled       *bool    `json:"enabled"`
			DecisionModel *string  `json:"decisionModel"`
			ResolverModel *string  `json:"resolverModel"`
			AckConfidence *float64 `json:"ackConfidence"`
			Run           bool     `json:"run"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
			writeError(w, http.StatusBadRequest, "bad json")
			return
		}
		if b.Enabled != nil || b.DecisionModel != nil || b.ResolverModel != nil || b.AckConfidence != nil {
			if _, err := a.updateReviewConfig(b.Enabled, b.DecisionModel, b.ResolverModel, b.AckConfidence); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
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
			out := a.reviewStatus()
			out["judged"] = n
			writeJSON(w, out)
			return
		}
	}
	writeJSON(w, a.reviewStatus())
}

func (a *api) updateReviewConfig(enabled *bool, decision, resolver *string, ack *float64) (ReviewConfig, error) {
	c := a.reviewConfig()
	if decision != nil {
		c.DecisionModel = strings.TrimSpace(*decision)
	}
	if resolver != nil {
		c.ResolverModel = strings.TrimSpace(*resolver)
	}
	if ack != nil {
		c.AckConfidence = *ack
	}
	if enabled != nil {
		c.Enabled = *enabled
	}
	if c.DecisionModel == "" {
		c.Enabled = false
	}
	if err := a.checkReviewConfig(c); err != nil {
		return c, err
	}
	raw, _ := json.Marshal(c)
	return c, a.store.SetSetting(reviewConfigKey, string(raw))
}

func (a *api) reviewStatus() map[string]any {
	c := a.reviewConfig()
	a.review.mu.Lock()
	lastErr := a.review.lastError
	a.review.mu.Unlock()
	return map[string]any{"enabled": c.Enabled, "decisionModel": c.DecisionModel, "resolverModel": c.ResolverModel,
		"ackConfidence": c.AckConfidence, "ready": c.DecisionModel != "" && a.reviewReady(c), "lastError": lastErr}
}

// toFieldPattern turns a drift path into a field filter pattern, as the
// dashboard's Block does: "messages[].x" → "messages.*.x", "{*}" → "*".
func toFieldPattern(p string) string {
	return strings.ReplaceAll(strings.ReplaceAll(p, "[]", ".*"), "{*}", "*")
}
