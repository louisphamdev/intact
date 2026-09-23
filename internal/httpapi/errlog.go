package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

// Every failed answer of a provider is kept for analysis, not only 429s:
// each attempt (also the ones intact failed over from), with its status,
// latency, headers, answer and the request that was sent, a signature that
// groups alike errors, and a first class. Kept 14 days, 20000 at most.

const (
	errKeepDays   = 14
	errKeepRows   = 20000
	errRespLimit  = 64 << 10
	errReqLimit   = 64 << 10
	fakeFastMs    = 800
	fakeQuotaLeft = 0.10
)

// Classes of an error.
const (
	ClassNetwork   = "network"
	ClassTimeout   = "timeout"
	ClassAuth      = "auth"
	ClassRejected  = "rejected"        // the provider refused the request (4xx)
	ClassRateLimit = "rate_limit"      // a real limit: the quota is low, or unknown
	ClassFake429   = "fake_rate_limit" // a 429 in a blink with quota left: refused content
	ClassServer    = "server"
	ClassOther     = "other"
)

var errInserts atomic.Int64

func classOf(status int) string {
	switch {
	case status == 0:
		return ClassNetwork
	case status == 401 || status == 403:
		return ClassAuth
	case status == 408 || status == 504 || status == 524:
		return ClassTimeout
	case status == 429:
		return ClassRateLimit
	case status >= 500:
		return ClassServer
	case status >= 400:
		return ClassRejected
	}
	return ClassOther
}

// errMessage reads the human message of an error answer.
func errMessage(b []byte) string {
	var d map[string]any
	if json.Unmarshal(b, &d) == nil {
		if e, ok := d["error"].(map[string]any); ok {
			if m := str(e["message"]); m != "" {
				return m
			}
		}
		for _, k := range []string{"message", "detail", "error", "error_description"} {
			if m := str(d[k]); m != "" {
				return m
			}
		}
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

var (
	sigID    = regexp.MustCompile(`(?i)\b[0-9a-f]{8,}(-[0-9a-f]{4,})*\b|req_[A-Za-z0-9]+|\b[A-Za-z0-9_-]{24,}\b`)
	sigNum   = regexp.MustCompile(`\d+(\.\d+)?`)
	sigSpace = regexp.MustCompile(`\s+`)
)

// errSignature groups alike errors: the status and the message with ids and
// numbers replaced.
func errSignature(status int, msg string) string {
	m := strings.ToLower(msg)
	m = sigID.ReplaceAllString(m, "#")
	m = sigNum.ReplaceAllString(m, "#")
	m = sigSpace.ReplaceAllString(strings.TrimSpace(m), " ")
	if len(m) > 160 {
		m = m[:160]
	}
	return strconv.Itoa(status) + " " + m
}

var keptHeaders = []string{"Retry-After", "Content-Type", "X-Request-Id", "Request-Id", "Cf-Ray", "X-Goog-Request-Id"}

func errHeaders(h http.Header) string {
	out := map[string]string{}
	for _, k := range keptHeaders {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "ratelimit") || strings.Contains(lk, "quota") {
			out[k] = strings.Join(v, ", ")
		}
	}
	if len(out) == 0 {
		return ""
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func clip(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// sendLogged sends like send and keeps any failure. An error answer's body is
// read and put back, so the caller relays it untouched.
func (a *api) sendLogged(r *http.Request, p provider.Provider, conn store.Connection, path, secret string, body []byte, model string) (*http.Response, error) {
	start := time.Now()
	resp, err := a.send(r, p, conn.Provider, path, secret, body)
	ms := time.Since(start).Milliseconds()
	e := store.UpstreamError{Provider: conn.Provider, Connection: conn.ID, Model: model, Client: clientOf(r),
		ClientKeyID: principalOf(r).keyID, Endpoint: path, LatencyMs: ms, QuotaLeft: -1, ReqBody: clip(body, errReqLimit)}
	switch {
	case err != nil:
		e.Status, e.Class, e.Message = 0, ClassNetwork, err.Error()
		if r.Context().Err() == nil && strings.Contains(err.Error(), "deadline") {
			e.Class = ClassTimeout
		}
	case resp.StatusCode >= 400:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, errRespLimit))
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(b))
		e.Status, e.Class, e.Message = resp.StatusCode, classOf(resp.StatusCode), errMessage(b)
		e.Headers, e.RespBody = errHeaders(resp.Header), clip(b, errRespLimit)
	default:
		return resp, err
	}
	if r.Context().Err() != nil && e.Status == 0 {
		return resp, err // the client went away; not the provider's error
	}
	e.Signature = errSignature(e.Status, e.Message)
	id, aerr := a.store.AddUpstreamError(e)
	if aerr == nil && e.Status == http.StatusTooManyRequests {
		go a.classify429(id, conn, model, ms)
	}
	if errInserts.Add(1)%200 == 0 {
		go a.store.PruneUpstreamErrors(errKeepDays, errKeepRows)
	}
	return resp, err
}

// classify429 reads the account's quota after a 429: a 429 answered in a
// blink while the model's quota is far from spent is the provider refusing
// the content (a fail-fast dressed as a rate limit), not a real limit.
func (a *api) classify429(id int64, conn store.Connection, model string, ms int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	left := quotaLeft(a.quotaFor(ctx, conn, false), model)
	class := ClassRateLimit
	if left > fakeQuotaLeft && ms < fakeFastMs {
		class = ClassFake429
	}
	a.store.SetErrorQuota(id, left, class)
}

// quotaLeft is the share of quota left for a model: its own window when the
// provider has one per model, else the tightest window; -1 when unknown.
// Windows arrive in name order, so the longest prefix has to win: the window
// of gemini-3.8-flash must not answer for gemini-3.8-flash-lite.
func quotaLeft(q AccountQuota, model string) float64 {
	best, exactModel, exactBase, prefix, prefixLen := -1.0, -1.0, -1.0, -1.0, -1
	base, _ := splitVariant(model)
	for _, w := range q.Windows {
		if w.UsedPct < 0 {
			continue
		}
		left := 1 - w.UsedPct/100
		name := strings.TrimPrefix(w.Name, "model ")
		switch {
		case name == w.Name:
			if best < 0 || left < best {
				best = left
			}
		case name == model:
			if exactModel < 0 || left < exactModel {
				exactModel = left
			}
		case base != "" && name == base:
			if exactBase < 0 || left < exactBase {
				exactBase = left
			}
		case strings.HasPrefix(model, name):
			if len(name) > prefixLen || (len(name) == prefixLen && (prefix < 0 || left < prefix)) {
				prefix, prefixLen = left, len(name)
			}
		}
	}
	switch {
	case exactModel >= 0:
		return exactModel
	case exactBase >= 0:
		return exactBase
	case prefix >= 0:
		return prefix
	}
	return best
}

// refusalsSince counts a provider's answers that refused a request since a
// time: the evidence a blacklist needs (not real limits, not the network).
func (a *api) refusalsSince(prov, since string) int {
	list, _ := a.store.ListUpstreamErrors(store.ErrorFilter{Provider: prov, Since: since, Limit: 5000})
	n := 0
	for _, e := range list {
		if e.Class == ClassRejected || e.Class == ClassFake429 {
			n++
		}
	}
	return n
}

// errorsList serves errors: ?provider=&class=&status=&signature=&since=&limit=.
func (a *api) errorsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	st, _ := strconv.Atoi(q.Get("status"))
	lim, _ := strconv.Atoi(q.Get("limit"))
	list, err := a.store.ListUpstreamErrors(store.ErrorFilter{Provider: q.Get("provider"), Class: q.Get("class"),
		Signature: q.Get("signature"), Since: q.Get("since"), Status: st, Limit: lim})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read errors")
		return
	}
	for i := range list {
		if !ownsError(r, list[i]) {
			list[i].ReqBody, list[i].RespBody = "", ""
		}
	}
	writeJSON(w, map[string]any{"errors": list})
}

// errorGet serves one error with its bodies. The request and response bodies
// can hold another client's prompt, so they are shown only to the owner (the
// dashboard session or the master token) or to the client that caused the error.
func (a *api) errorGet(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	e, err := a.store.GetUpstreamError(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such error")
		return
	}
	if !ownsError(r, e) {
		e.ReqBody, e.RespBody = "", ""
	}
	writeJSON(w, e)
}

// ownsError reports whether the caller may read an error's stored bodies: an
// admin (the dashboard session or the master token) may read every error; a
// dashboard key may read only the errors its own key id caused. An error
// stored before the key id was recorded belongs to no key.
func ownsError(r *http.Request, e store.UpstreamError) bool {
	p := principalOf(r)
	return p.admin || (e.ClientKeyID != "" && e.ClientKeyID == p.keyID)
}

// ErrorGroup is the errors of one signature.
type ErrorGroup struct {
	Signature string         `json:"signature"`
	Provider  string         `json:"provider"`
	Status    int            `json:"status"`
	Classes   map[string]int `json:"classes"`
	Count     int            `json:"count"`
	First     string         `json:"first"`
	Last      string         `json:"last"`
	Models    []string       `json:"models"`
	Message   string         `json:"message"`
	LastID    int64          `json:"lastId"`
	MedianMs  int64          `json:"medianMs"`
}

func (a *api) errorGroups(f store.ErrorFilter) ([]ErrorGroup, error) {
	f.Limit = 5000
	list, err := a.store.ListUpstreamErrors(f)
	if err != nil {
		return nil, err
	}
	idx := map[string]*ErrorGroup{}
	lat := map[string][]int64{}
	var order []string
	for _, e := range list { // newest first
		k := e.Provider + "|" + e.Signature
		g := idx[k]
		if g == nil {
			g = &ErrorGroup{Signature: e.Signature, Provider: e.Provider, Status: e.Status, Classes: map[string]int{},
				Last: e.At, Message: e.Message, LastID: e.ID}
			idx[k] = g
			order = append(order, k)
		}
		g.Count++
		g.First = e.At
		g.Classes[e.Class]++
		if e.Model != "" && !contains(g.Models, e.Model) && len(g.Models) < 8 {
			g.Models = append(g.Models, e.Model)
		}
		lat[k] = append(lat[k], e.LatencyMs)
	}
	out := make([]ErrorGroup, 0, len(order))
	for _, k := range order {
		g := idx[k]
		l := lat[k]
		sortInt64(l)
		g.MedianMs = l[len(l)/2]
		out = append(out, *g)
	}
	return out, nil
}

func sortInt64(l []int64) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j] < l[j-1]; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

// errorStats serves the errors grouped by signature: ?provider=&since=.
func (a *api) errorStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	g, err := a.errorGroups(store.ErrorFilter{Provider: q.Get("provider"), Since: q.Get("since")})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read errors")
		return
	}
	writeJSON(w, map[string]any{"groups": g})
}
