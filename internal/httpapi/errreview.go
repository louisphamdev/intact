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
	"time"

	"github.com/louisphamdev/intact/internal/filter"
	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

// The error review settles recurring provider errors with no person
// involved. A chat model reads a group of alike errors (the newest request
// that failed, the answer, what drift saw change, the rules in place) and
// proposes an action; intact checks it before acting:
//
//   - blacklist: each candidate rule must remove something from the failing
//     request, and the request, replayed without it, must be accepted. When
//     the request cannot be replayed, the rule's name must appear in the
//     provider's message.
//   - disable_model: the provider says it does not serve the model, and every
//     error of the group is on that model.
//   - disable_account: sign-in errors of one account for ten minutes or more.
//   - ignore: nothing to change (the client's own mistake, a passing failure,
//     a real limit).
//
// Before asking the model, intact replays the request: when it now succeeds
// (a passing failure) or the blacklist in place already fixes it, the group
// is closed without a model call. A false 429 that the replay reproduces is
// searched by replay in the system prompt (see bisectSystem) before the model.

const (
	errReviewKey      = "error-review-config"
	errReviewBatch    = 5
	errReviewSnooze   = 24 * time.Hour
	errBurstCount     = 20
	errBurstWindow    = time.Hour
	defaultErrMin     = 3
	errReplayTimeout  = 90 * time.Second
	errPromptReqLimit = 14 << 10
	// reviewMaxTokens leaves room for a thinking model's reasoning before
	// its JSON answer; 1024 cut Gemini 3.8 Flash's answer short.
	reviewMaxTokens = 8192
)

// ErrorReviewConfig is the error review's settings.
//   - Model: any chat model intact serves. Without one, the review cannot be on.
//   - MinErrors: the errors a group needs, within a day, to be reviewed.
//   - Replay: replay failing requests to check a fix (and to spot a passing
//     failure). It costs a provider call per check.
type ErrorReviewConfig struct {
	Enabled   bool   `json:"enabled"`
	Model     string `json:"model"`
	MinErrors int    `json:"minErrors"`
	Replay    bool   `json:"replay"`
}

func (a *api) errReviewConfig() ErrorReviewConfig {
	c := ErrorReviewConfig{Replay: true}
	if v, _ := a.store.GetSetting(errReviewKey); v != "" {
		json.Unmarshal([]byte(v), &c)
	}
	if c.MinErrors <= 0 {
		c.MinErrors = defaultErrMin
	}
	if c.Model == "" {
		c.Enabled = false
	}
	return c
}

func (a *api) checkErrReviewConfig(c ErrorReviewConfig) error {
	if c.Model != "" {
		prov, _ := a.splitModel(c.Model)
		p, ok := provider.Lookup(prov)
		if prov == "" || !ok || p.API == "typesafe" {
			return errors.New("model: a chat model intact serves, <provider>/<model> (such as antigravity/gemini-3.8-flash)")
		}
	}
	if c.Enabled && c.Model == "" {
		return errors.New("the error review needs a model before it can be on")
	}
	if c.MinErrors < 1 || c.MinErrors > 1000 {
		return errors.New("minErrors: 1 to 1000")
	}
	return nil
}

var reviewedClasses = map[string]bool{ClassRejected: true, ClassFake429: true, ClassAuth: true}

func reviewable(g ErrorGroup) bool {
	for c := range g.Classes {
		if reviewedClasses[c] {
			return true
		}
	}
	return false
}

// pendingGroups returns the groups due for a review: enough errors in a day
// and never judged; errors again after an applied fix; or still failing a
// day after being left alone.
func (a *api) pendingGroups(cfg ErrorReviewConfig) ([]ErrorGroup, error) {
	since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	groups, err := a.errorGroups(store.ErrorFilter{Since: since})
	if err != nil {
		return nil, err
	}
	verdicts, err := a.store.ErrorVerdicts()
	if err != nil {
		return nil, err
	}
	var out []ErrorGroup
	for _, g := range groups {
		if !reviewable(g) || g.Count < cfg.MinErrors {
			continue
		}
		v, seen := verdicts[g.Provider+"|"+g.Signature]
		if seen {
			if g.Last <= v.At {
				continue
			}
			after, _ := a.store.ListUpstreamErrors(store.ErrorFilter{Provider: g.Provider, Signature: g.Signature, Since: v.At, Limit: 1000})
			n := 0
			for _, e := range after {
				if e.At > v.At {
					n++
				}
			}
			at, _ := time.Parse(time.RFC3339, v.At)
			fixed := v.Applied && v.Action != "ignore"
			if n < cfg.MinErrors || (!fixed && time.Since(at) < errReviewSnooze && !a.guessCanBeChecked(g, v)) {
				continue
			}
		}
		out = append(out, g)
		if len(out) == errReviewBatch {
			break
		}
	}
	return out, nil
}

// guessCanBeChecked reports a false 429 that a model judged without a replay,
// while its newest error can now be replayed: the snooze would keep a guess.
func (a *api) guessCanBeChecked(g ErrorGroup, v store.ErrorVerdict) bool {
	if g.Classes[ClassFake429] == 0 || v.Applied || v.Replayed || v.By == "intact" {
		return false
	}
	e, err := a.store.GetUpstreamError(g.LastID)
	return err == nil && a.replayBody(e) != ""
}

// replay sends a request body again, as it was sent, through the account
// that failed. It returns the status and the start of the answer.
func (a *api) replay(ctx context.Context, e store.UpstreamError, body []byte) (int, string, error) {
	list, _ := a.store.ListConnections()
	var conn store.Connection
	for _, c := range list {
		if c.ID == e.Connection {
			conn = c
		}
	}
	if conn.ID == "" || !conn.IsActive {
		return 0, "", errors.New("the account is gone or off")
	}
	p, ok := a.providerFor(conn)
	if !ok {
		return 0, "", errors.New("no provider")
	}
	secret, err := a.secretFor(ctx, conn.ID)
	if err == nil {
		secret, err = a.exchanged(ctx, p, conn.ID, secret)
	}
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, errReplayTimeout)
	defer cancel()
	// The stored endpoint carries its own query; the request must not add it
	// again.
	r := withPrincipal(httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/replay", nil), intactJob)
	resp, err := a.send(r, p, conn.Provider, e.Endpoint, secret, body)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, errMessage(b), nil
}

func replayable(e store.UpstreamError) bool {
	return e.ReqBody != "" && !(len(e.ReqBody) >= errReqLimit && strings.HasSuffix(e.ReqBody, "…"))
}

// replayBody is the request to replay: the error's own when it was kept whole,
// else the full request kept for its group.
func (a *api) replayBody(e store.UpstreamError) string {
	if replayable(e) {
		return e.ReqBody
	}
	full, _ := a.store.ErrorBody(e.Provider, e.Signature)
	return full
}

// sketchJSON shortens a request for a prompt: long strings cut, long
// arrays reduced to their first and last items.
func sketchJSON(raw string, limit int) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		if len(raw) > limit {
			return raw[:limit] + "…"
		}
		return raw
	}
	var walk func(v any, strMax, arrMax int) any
	walk = func(v any, strMax, arrMax int) any {
		switch t := v.(type) {
		case string:
			if len(t) > strMax {
				return fmt.Sprintf("%s…(+%d chars)", t[:strMax], len(t)-strMax)
			}
		case []any:
			if len(t) > arrMax {
				keep := append(append([]any{}, t[:1]...), fmt.Sprintf("…(%d more items)…", len(t)-arrMax))
				t = append(keep, t[len(t)-(arrMax-1):]...)
			}
			out := make([]any, len(t))
			for i, x := range t {
				out[i] = walk(x, strMax, arrMax)
			}
			return out
		case map[string]any:
			out := map[string]any{}
			for k, x := range t {
				out[k] = walk(x, strMax, arrMax)
			}
			return out
		}
		return v
	}
	for _, lim := range [][2]int{{300, 8}, {120, 5}, {60, 3}} {
		b, _ := json.Marshal(walk(v, lim[0], lim[1]))
		if len(b) <= limit {
			return string(b)
		}
	}
	b, _ := json.Marshal(walk(v, 40, 2))
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}
	return string(b)
}

const errReviewPrompt = `You fix errors that providers answer to intact, an LLM gateway. Clients send requests to intact, which forwards them to a provider account. You see one group of alike errors from one provider: the newest failing request exactly as it was sent to the provider (long text shortened), the provider's answer, and facts intact knows.

Choose one action:
- blacklist: the request carries something this provider refuses. intact will strip it from every request to this provider. Give up to 3 candidates, most likely first, written for the request as shown:
  - {"kind":"field","pattern":"<dot path; * matches any key or array item>"} such as "request.generationConfig.foo" or "messages.*.cache_control";
  - {"kind":"schema","pattern":"<key>"}: a key removed at every depth of the tools' JSON schemas, such as "$schema";
  - {"kind":"system","pattern":"<regular expression>"}: system prompt lines to remove;
  - {"kind":"header","pattern":"<header name>"}.
  Never strip what the request needs (model, messages, contents, tools, the system prompt as a whole).
- disable_model: the provider no longer serves this model (not found, unsupported, deprecated).
- disable_account: this account's credentials are revoked, or the account is suspended; not a passing sign-in hiccup.
- ignore: nothing intact should change: the client sent a request only the client can fix (a malformed tool schema, a prompt too long), a passing failure, or a real rate limit.

class fake_rate_limit is a 429 answered at once while the account's quota was left: often the provider refused the content, not a rate limit. When the replay says the request fails again and the rules below find nothing, it can still be a real limit that the provider names (a shared pool, an upstream provider). Most often the provider refuses a sentence of the system prompt that names the client, its maker or its model, such as "You are Codex, an agent based on GPT-5." or "You are a Claude agent, built on Anthropic's Claude Agent SDK."; system_prompt_sentences lists them. Give system candidates that match only the smallest part of that sentence that names the client, words joined by \s+, such as "\\bbuilt\\s+on\\s+Anthropic's\\b"; never a pattern that matches ordinary instructions. Otherwise it is an unknown field or header.
intact checks you: a blacklist rule is kept only if the replayed request is accepted without it.

Answer with one JSON object and nothing else: {"cause":"<a few words>","action":"blacklist"|"disable_model"|"disable_account"|"ignore","candidates":[{"kind":"...","pattern":"..."}],"reason":"<one short sentence>"}`

type errProposal struct {
	Cause      string `json:"cause"`
	Action     string `json:"action"`
	Candidates []struct {
		Kind    string `json:"kind"`
		Pattern string `json:"pattern"`
	} `json:"candidates"`
	Reason string `json:"reason"`
}

// chatOnce sends one chat request through intact's own /v1 and returns the
// answer's text.
func (a *api) chatOnce(ctx context.Context, model, system, user string, maxTokens int) (string, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": maxTokens,
		"messages": []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": user}}})
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req := withPrincipal(httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))), intactJob)
	req.SetPathValue("path", "chat/completions")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.v1(rec, req)
	raw, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK {
		return "", fmt.Errorf("%s: %d %s", model, rec.Code, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
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
		return "", fmt.Errorf("%s gave no answer", model)
	}
	return d.Choices[0].Message.Content, nil
}

// errFacts is what intact knows of a group, for the model.
func (a *api) errFacts(g ErrorGroup, e store.UpstreamError, body, reproduced string) map[string]any {
	first, _ := time.Parse(time.RFC3339, g.First)
	var drift []string
	if list, err := a.store.ShapeChangesSince(first.Add(-48 * time.Hour).Format(time.RFC3339)); err == nil {
		for _, c := range list {
			if c.Provider == g.Provider && c.Direction == "request" && (c.Kind == "added" || c.Kind == "type") && c.At <= g.Last && len(drift) < 20 {
				drift = append(drift, c.Path+" ("+c.Kind+" "+c.At+")")
			}
		}
	}
	var rules []string
	for _, r := range a.rulesFor(g.Provider) {
		rules = append(rules, r.Kind+": "+r.Pattern)
	}
	answer := e.RespBody
	if len(answer) > 2000 {
		answer = answer[:2000] + "…"
	}
	return map[string]any{
		"provider": g.Provider, "status": g.Status, "classes": g.Classes, "errors_in_24h": g.Count, "first": g.First, "last": g.Last,
		"models": g.Models, "median_latency_ms": g.MedianMs, "quota_left_at_newest": e.QuotaLeft, "client_of_newest": e.Client,
		"endpoint": e.Endpoint, "message": e.Message, "answer": answer, "answer_headers": e.Headers,
		"request_fields_new_to_this_provider_before_the_errors": drift, "blacklist_rules_already_in_place": rules,
		"replay_of_the_failing_request": reproduced,
		"system_prompt_sentences":       promptSentences(body),
		"request_sent":                  sketchJSON(e.ReqBody, errPromptReqLimit),
	}
}

// protectedPattern reports a rule that would strip what a request needs. The
// body kinds ask the shared content guard; only the header list is its own.
func protectedPattern(kind, pattern string) bool {
	switch kind {
	case filter.Field:
		return essentialField(pattern)
	case filter.System:
		return catchAllSystem(pattern)
	case filter.Header:
		h := strings.ToLower(pattern)
		return h == "authorization" || h == "content-type" || h == "x-api-key" || h == "host"
	}
	return false
}

// reviewErrorGroup judges one group and acts on the verdict.
func (a *api) reviewErrorGroup(ctx context.Context, cfg ErrorReviewConfig, g ErrorGroup) (store.ErrorVerdict, error) {
	v := store.ErrorVerdict{Provider: g.Provider, Signature: g.Signature, Errors: g.Count, LastError: g.Last}
	// Another pass may have judged this group since pendingGroups read it.
	if verdicts, err := a.store.ErrorVerdicts(); err == nil {
		if last, ok := verdicts[g.Provider+"|"+g.Signature]; ok && g.Last <= last.At {
			return v, errGroupJudged
		}
	}
	e, err := a.store.GetUpstreamError(g.LastID)
	if err != nil {
		return v, err
	}
	body := a.replayBody(e)
	canReplay := cfg.Replay && body != "" && (g.Classes[ClassRejected] > 0 || g.Classes[ClassFake429] > 0)
	reproduced := "not replayed"
	if canReplay {
		req := []byte(body)
		if now, changed := filter.Apply(req, a.rulesFor(g.Provider)); changed {
			if st, _, err := a.replay(ctx, e, now); err == nil && st < 400 {
				v.Action, v.By, v.Verified = "ignore", "intact", true
				v.Cause, v.Note = "fixed already", "the blacklist in place now removes the cause: the request is accepted"
				return a.store.AddErrorVerdict(v)
			}
		}
		st, msg, err := a.replay(ctx, e, req)
		switch {
		case err != nil:
			canReplay, reproduced = false, "could not replay: "+err.Error()
		case st >= 400 && st != g.Status:
			// Another error than the group's: the replay does not stand for it.
			canReplay, reproduced = false, fmt.Sprintf("the replay failed differently: %d %s", st, truncate(msg, 200))
		case st < 400:
			v.Action, v.By, v.Cause = "ignore", "intact", "passing failure"
			v.Note = fmt.Sprintf("the same request is accepted now (%d)", st)
			return a.store.AddErrorVerdict(v)
		default:
			reproduced = fmt.Sprintf("the same request fails again: %d %s", st, msg)
			if g.Classes[ClassFake429] > 0 && a.settleFake429(ctx, &v, g, e, req) {
				saved, err := a.store.AddErrorVerdict(v)
				if err == nil {
					a.alertVerdict(saved, g)
				}
				return saved, err
			}
		}
	}
	v.Replayed = canReplay
	facts, _ := json.Marshal(a.errFacts(g, e, body, reproduced))
	text, err := a.chatOnce(ctx, cfg.Model, errReviewPrompt, string(facts), reviewMaxTokens)
	if err != nil {
		return v, err
	}
	var p errProposal
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j <= i || json.Unmarshal([]byte(text[i:j+1]), &p) != nil || p.Action == "" {
		return v, fmt.Errorf("%s answered no verdict: %.120s", cfg.Model, text)
	}
	v.By, v.Cause, v.Reason, v.Action = cfg.Model, p.Cause, p.Reason, p.Action
	switch {
	case p.Action == "ignore":
	case p.Action != "blacklist" && p.Action != "disable_model" && p.Action != "disable_account":
		v.Action, v.Note = "ignore", "unknown action "+p.Action
	case needsOwnerApproval(e):
		v.Note = "the owner must approve: the failing request was sent by a dashboard key, so its text can steer the model"
	case p.Action == "blacklist":
		a.tryBlacklist(ctx, &v, e, []byte(body), p, canReplay)
	case p.Action == "disable_model":
		a.tryDisableModel(&v, g, e)
	default:
		a.tryDisableAccount(&v, g, cfg)
	}
	saved, err := a.store.AddErrorVerdict(v)
	if err == nil && v.Action != "ignore" {
		a.alertVerdict(saved, g)
	}
	return saved, err
}

func (a *api) tryBlacklist(ctx context.Context, v *store.ErrorVerdict, e store.UpstreamError, req []byte, p errProposal, canReplay bool) {
	var notes []string
	for n, c := range p.Candidates {
		if n == 3 {
			break
		}
		name := c.Kind + " " + c.Pattern
		rule, err := filter.Compile(c.Kind, c.Pattern)
		if err != nil {
			notes = append(notes, name+": "+err.Error())
			continue
		}
		if protectedPattern(c.Kind, rule.Pattern) {
			notes = append(notes, name+": the request needs it")
			continue
		}
		verified := false
		if c.Kind != filter.Header {
			out, changed := filter.Apply(req, []filter.Rule{rule})
			if !changed {
				notes = append(notes, name+": removes nothing from the failing request")
				continue
			}
			if canReplay {
				st, msg, err := a.replay(ctx, e, out)
				if err != nil || st >= 400 {
					notes = append(notes, fmt.Sprintf("%s: still refused (%d %s)", name, st, truncate(strings.Join(strings.Fields(msg), " "), 80)))
					continue
				}
				verified = true
			}
		}
		if !verified {
			key := rule.Pattern
			if k := strings.LastIndexAny(key, ".*"); k >= 0 && c.Kind == filter.Field {
				key = key[k+1:]
			}
			if key == "" || !strings.Contains(strings.ToLower(e.Message+e.RespBody), strings.ToLower(key)) {
				notes = append(notes, name+": not checked, and the provider's message does not name it")
				continue
			}
		}
		if a.filterInPlace(e.Provider, c.Kind, rule.Pattern) {
			v.Applied, v.Verified, v.Detail = true, verified, c.Kind+": "+rule.Pattern
			notes = append(notes, name+": the rule is already in place")
			break
		}
		if _, err := a.putFilter(store.Filter{Provider: e.Provider, Kind: c.Kind, Pattern: rule.Pattern,
			Note: "error review: " + truncate(e.Signature, 80), Enabled: true}); err != nil {
			notes = append(notes, name+": "+err.Error())
			continue
		}
		v.Applied, v.Verified, v.Detail = true, verified, c.Kind+": "+rule.Pattern
		if verified {
			notes = append(notes, name+": the request is accepted without it")
		} else {
			notes = append(notes, name+": named in the provider's message")
		}
		break
	}
	if len(p.Candidates) == 0 {
		notes = append(notes, "no candidate given")
	}
	v.Note = strings.Join(notes, "; ")
}

func (a *api) tryDisableModel(v *store.ErrorVerdict, g ErrorGroup, e store.UpstreamError) {
	msg := strings.ToLower(e.Message + " " + e.RespBody)
	switch {
	case len(g.Models) != 1:
		v.Note = "not switched off: the errors are on several models"
		return
	case g.Status != 400 && g.Status != 404 && g.Status != 403:
		v.Note = fmt.Sprintf("not switched off: status %d does not say the model is gone", g.Status)
		return
	case !strings.Contains(msg, "model"):
		v.Note = "not switched off: the provider's message does not speak of the model"
		return
	}
	model := g.Models[0]
	known := map[string]bool{}
	if list, err := a.store.ListModels(g.Provider); err == nil {
		for _, m := range list {
			known[m.Model] = true
		}
	}
	if !known[model] {
		if base, _ := splitVariant(model); known[base] {
			model = base
		} else {
			v.Note = "not switched off: " + model + " is not in the model table"
			return
		}
	}
	if _, err := a.store.SetModelsActive(g.Provider, []string{model}, false); err != nil {
		v.Note = "switching off failed: " + err.Error()
		return
	}
	v.Applied, v.Detail, v.Note = true, g.Provider+"/"+model, "switched off in the model table"
}

func (a *api) tryDisableAccount(v *store.ErrorVerdict, g ErrorGroup, cfg ErrorReviewConfig) {
	list, _ := a.store.ListUpstreamErrors(store.ErrorFilter{Provider: g.Provider, Signature: g.Signature, Class: ClassAuth,
		Since: time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339), Limit: 1000})
	span := map[string][2]string{}
	count := map[string]int{}
	for _, e := range list { // newest first
		s := span[e.Connection]
		if s[1] == "" {
			s[1] = e.At
		}
		s[0] = e.At
		span[e.Connection] = s
		count[e.Connection]++
	}
	best := ""
	for id, n := range count {
		if n >= cfg.MinErrors && (best == "" || n > count[best]) {
			best = id
		}
	}
	if best == "" {
		v.Note = "not switched off: no account has enough sign-in errors"
		return
	}
	from, _ := time.Parse(time.RFC3339, span[best][0])
	to, _ := time.Parse(time.RFC3339, span[best][1])
	if to.Sub(from) < 10*time.Minute {
		v.Note = "not switched off: the sign-in errors span less than ten minutes"
		return
	}
	if err := a.store.SetActive(best, false); err != nil {
		v.Note = "switching off failed: " + err.Error()
		return
	}
	label := best
	if conns, err := a.store.ListConnections(); err == nil {
		for _, c := range conns {
			if c.ID == best && c.Label != "" {
				label = c.Label
			}
		}
	}
	v.Applied, v.Detail = true, label+" ("+best+")"
	v.Note = fmt.Sprintf("switched off after %d sign-in errors over %s", count[best], to.Sub(from).Round(time.Minute))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (a *api) alertVerdict(v store.ErrorVerdict, g ErrorGroup) {
	lines := []string{"Provider: " + pnameOf(v.Provider), "Error: " + truncate(g.Message, 200),
		fmt.Sprintf("Errors in 24h: %d", g.Count)}
	if v.Reason != "" {
		lines = append(lines, "Why: "+v.Reason)
	}
	if v.Note != "" {
		lines = append(lines, "Checked: "+truncate(v.Note, 300))
	}
	lines = append(lines, "By: "+v.By)
	if v.Applied {
		title := map[string]string{"blacklist": "Blacklisted " + v.Detail, "disable_model": "Model switched off: " + v.Detail,
			"disable_account": "Account switched off: " + v.Detail}[v.Action]
		a.notify(EventErrorAction, "", 0, notifyMsg{Title: title, Lines: lines, Path: "#/errors/groups"})
		return
	}
	a.notify(EventErrorUnresolved, "unresolved|"+v.Provider+"|"+v.Signature, errReviewSnooze,
		notifyMsg{Title: "Error not fixed (" + v.Action + " refused)", Lines: lines, Path: "#/errors/groups"})
}

func pnameOf(id string) string {
	if d, ok := provider.Declared(id); ok && d.Name != "" {
		return d.Name
	}
	return id
}

// reviewErrors judges the groups due. It stops at the first failure of the
// model.
func (a *api) reviewErrors(ctx context.Context) (int, error) {
	if !a.errReview.run.TryLock() {
		return 0, errReviewRunning
	}
	defer a.errReview.run.Unlock()
	cfg := a.errReviewConfig()
	if !cfg.Enabled {
		return 0, nil
	}
	groups, err := a.pendingGroups(cfg)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, g := range groups {
		_, err := a.reviewErrorGroup(ctx, cfg, g)
		if errors.Is(err, errGroupJudged) {
			continue
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// errGroupJudged reports a group another pass settled first. The group is
// skipped, not counted, and it is not a failure.
var errGroupJudged = errors.New("the group was judged by another pass")

// needsOwnerApproval reports an error whose request text can steer the model: a
// dashboard key sent it, or it came from the review's own model call. The
// verdict is stored, and nothing acts on it.
func needsOwnerApproval(e store.UpstreamError) bool {
	return e.ClientKeyID != "" || e.Client == intactJob.name
}

// alertBursts tells the channels of a group of errors growing fast, once in
// six hours per group.
func (a *api) alertBursts() {
	groups, err := a.errorGroups(store.ErrorFilter{Since: time.Now().UTC().Add(-errBurstWindow).Format(time.RFC3339)})
	if err != nil {
		return
	}
	for _, g := range groups {
		if g.Count < errBurstCount {
			continue
		}
		classes := []string{}
		for c, n := range g.Classes {
			classes = append(classes, fmt.Sprintf("%s ×%d", c, n))
		}
		a.notify(EventErrorBurst, "burst|"+g.Provider+"|"+g.Signature, 6*time.Hour, notifyMsg{
			Title: fmt.Sprintf("%d errors in an hour from %s", g.Count, pnameOf(g.Provider)),
			Lines: []string{"Error: " + truncate(g.Message, 200), "Classes: " + strings.Join(classes, ", "),
				"Models: " + strings.Join(g.Models, ", ")},
			Path: "#/errors/groups"})
	}
}

// errorReviewLoop runs the error review and the burst alerts.
func (a *api) errorReviewLoop() {
	time.Sleep(2 * time.Minute)
	for {
		a.alertBursts()
		a.errReview.mu.Lock()
		paused := time.Now().Before(a.errReview.pauseTill)
		a.errReview.mu.Unlock()
		if !paused {
			a.errorReviewOnce()
		}
		time.Sleep(reviewEvery)
	}
}

// errorReviewOnce runs one pass and records a failure of the model. A pass
// that is already running is not a failure, so it neither pauses nor alerts.
func (a *api) errorReviewOnce() {
	n, err := a.reviewErrors(context.Background())
	switch {
	case errors.Is(err, errReviewRunning):
	case err != nil:
		a.pauseErrReview(err)
	case n > 0:
		a.errReview.mu.Lock()
		a.errReview.lastError = ""
		a.errReview.mu.Unlock()
		log.Printf("error review: judged %d group(s)", n)
	}
}

func (a *api) pauseErrReview(err error) {
	log.Printf("error review: %v", err)
	a.errReview.mu.Lock()
	a.errReview.pauseTill, a.errReview.lastError = time.Now().Add(reviewBackoff), err.Error()
	a.errReview.mu.Unlock()
	a.notify(EventReviewPaused, "paused|errors", 6*time.Hour, notifyMsg{Title: "Error review paused for 10 minutes",
		Lines: []string{truncate(err.Error(), 300)}, Path: "#/errors"})
}

// errorReview serves the review's config and state. POST takes
// {"enabled","model","minErrors","replay"} (fields left out keep their
// value) and {"run":true} to judge the groups due now.
func (a *api) errorReview(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var b struct {
			Enabled   *bool   `json:"enabled"`
			Model     *string `json:"model"`
			MinErrors *int    `json:"minErrors"`
			Replay    *bool   `json:"replay"`
			Run       bool    `json:"run"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
			writeError(w, http.StatusBadRequest, "bad json")
			return
		}
		c := a.errReviewConfig()
		if b.Model != nil {
			c.Model = strings.TrimSpace(*b.Model)
		}
		if b.MinErrors != nil {
			c.MinErrors = *b.MinErrors
		}
		if b.Replay != nil {
			c.Replay = *b.Replay
		}
		if b.Enabled != nil {
			c.Enabled = *b.Enabled
		}
		if c.Model == "" {
			c.Enabled = false
		}
		if err := a.checkErrReviewConfig(c); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		raw, _ := json.Marshal(c)
		a.store.SetSetting(errReviewKey, string(raw))
		if b.Run {
			n, err := a.reviewErrors(r.Context())
			if errors.Is(err, errReviewRunning) {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			if err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			a.errReview.mu.Lock()
			a.errReview.pauseTill, a.errReview.lastError = time.Time{}, ""
			a.errReview.mu.Unlock()
			out := a.errReviewStatus()
			out["judged"] = n
			writeJSON(w, out)
			return
		}
	}
	writeJSON(w, a.errReviewStatus())
}

func (a *api) errReviewStatus() map[string]any {
	c := a.errReviewConfig()
	a.errReview.mu.Lock()
	lastErr := a.errReview.lastError
	a.errReview.mu.Unlock()
	ready := false
	if prov, _ := a.splitModel(c.Model); prov != "" {
		ready = len(a.activeConnections(prov)) > 0
	}
	return map[string]any{"enabled": c.Enabled, "model": c.Model, "minErrors": c.MinErrors, "replay": c.Replay,
		"ready": ready, "lastError": lastErr}
}

// errorVerdicts serves the review's verdicts, newest first: ?limit=.
func (a *api) errorVerdicts(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListErrorVerdicts(0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read verdicts")
		return
	}
	writeJSON(w, map[string]any{"verdicts": list})
}
