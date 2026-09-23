package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/upstream"
)

// Quota endpoints, as the tools read them. Variables so a test can redirect.
var (
	codexUsageURL       = "https://chatgpt.com/backend-api/wham/usage"
	codexCreditsURL     = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	claudeUsageURL      = "https://api.anthropic.com/api/oauth/usage"
	copilotUserURL      = "https://api.github.com/copilot_internal/user"
	openrouterKeyURL    = "https://openrouter.ai/api/v1/key"
	groqModelsURL       = "https://api.groq.com/openai/v1/models"
	antigravityQuotaURL = "https://daily-cloudcode-pa.googleapis.com"
)

func init() {
	quotaFetchers["codex"] = quotaCodex
	quotaFetchers["claude"] = quotaClaude
	quotaFetchers["github"] = quotaCopilot
	quotaFetchers["antigravity"] = quotaAntigravity
	quotaFetchers["groq"] = quotaGroq
	quotaFetchers["openrouter"] = quotaOpenRouter
}

// fetchJSON sends one request and decodes a JSON answer; it returns the
// response headers too.
func fetchJSON(ctx context.Context, method, url string, headers map[string]string, body any, out any) (http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := upstream.Do(ctx, req, 1)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return resp.Header, fmt.Errorf("%d: %s", resp.StatusCode, msg)
	}
	if out != nil {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(out); err != nil {
			return resp.Header, fmt.Errorf("unreadable answer: %w", err)
		}
	}
	return resp.Header, nil
}

func jnum(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func jobj(v any) map[string]any { m, _ := v.(map[string]any); return m }

// resetTime reads a reset as ISO text or epoch seconds/milliseconds.
func resetTime(v any) string {
	if s, ok := v.(string); ok && s != "" {
		if _, err := time.Parse(time.RFC3339, s); err == nil {
			return s
		}
	}
	if f, ok := jnum(v); ok && f > 0 {
		if f > 1e12 {
			return time.UnixMilli(int64(f)).UTC().Format(time.RFC3339)
		}
		return time.Unix(int64(f), 0).UTC().Format(time.RFC3339)
	}
	return ""
}

func clampPct(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 100 {
		return 100
	}
	return f
}

// quotaCodex reads the ChatGPT plan's Codex windows.
func quotaCodex(a *api, ctx context.Context, c store.Connection, token string) (AccountQuota, error) {
	var d map[string]any
	h := map[string]string{"Authorization": "Bearer " + token}
	if id := chatgptAccountID(token); id != "" {
		h["ChatGPT-Account-ID"] = id
	}
	u := codexUsageURL
	cu := codexCreditsURL
	if over, ok := a.baseOverride["codex"]; ok {
		u = over + "/backend-api/wham/usage"
		cu = over + "/backend-api/wham/rate-limit-reset-credits"
	}
	if _, err := fetchJSON(ctx, "GET", u, h, nil, &d); err != nil {
		return AccountQuota{}, err
	}
	q := AccountQuota{Windows: []QuotaWindow{}, Resets: []QuotaReset{}}
	if s, ok := d["plan_type"].(string); ok {
		q.Plan = s
	}
	window := func(prefix string, rl map[string]any) {
		if inner := jobj(rl["rate_limit"]); inner != nil {
			rl = inner
		}
		for _, k := range []string{"primary_window", "secondary_window"} {
			w := jobj(rl[k])
			if w == nil {
				w = jobj(rl[strings.TrimSuffix(k, "_window")])
			}
			if w == nil {
				continue
			}
			used, ok := jnum(w["used_percent"])
			if !ok {
				used, _ = jnum(w["percent_used"])
			}
			name := map[string]string{"primary_window": "session", "secondary_window": "weekly"}[k]
			if secs, ok := jnum(w["limit_window_seconds"]); ok && secs > 0 {
				name = windowName(int(secs / 60))
			}
			if prefix != "" {
				name = prefix + " " + name
			}
			reset := resetTime(w["reset_at"])
			if reset == "" {
				reset = resetTime(w["resets_at"])
			}
			q.Windows = append(q.Windows, QuotaWindow{Name: name, UsedPct: clampPct(used), ResetAt: reset})
		}
	}
	rl := jobj(d["rate_limit"])
	if rl == nil {
		rl = jobj(d["rate_limits"])
	}
	if rl == nil {
		rl = jobj(jobj(d["rate_limits_by_limit_id"])["codex"])
	}
	if rl != nil {
		window("", rl)
	}
	if rr := jobj(d["code_review_rate_limit"]); rr != nil {
		window("review", rr)
	}
	for _, x := range func() []any { l, _ := d["additional_rate_limits"].([]any); return l }() {
		m := jobj(x)
		name, _ := m["limit_name"].(string)
		if name == "" {
			name, _ = m["metered_feature"].(string)
		}
		if name != "" {
			window(name, m)
		}
	}

	availCount, _ := jnum(jobj(d["rate_limit_reset_credits"])["available_count"])
	if availCount > 0 {
		var cd map[string]any
		if _, err := fetchJSON(ctx, "GET", cu, h, nil, &cd); err != nil {
			q.ResetsError = err.Error()
		} else {
			var rollingWindows []string
			for _, w := range q.Windows {
				if !strings.Contains(w.Name, " ") {
					rollingWindows = append(rollingWindows, w.Name)
				}
			}
			q.Resets = codexResetRows(cd, time.Now().UTC(), rollingWindows)
		}
	}
	if q.Resets == nil {
		q.Resets = []QuotaReset{}
	}
	return q, nil
}

// quotaClaude reads the Claude plan's 5-hour and weekly utilization.
func quotaClaude(a *api, ctx context.Context, c store.Connection, token string) (AccountQuota, error) {
	h := map[string]string{
		"Authorization":     "Bearer " + token,
		"Anthropic-Beta":    "oauth-2025-04-20",
		"Anthropic-Version": "2023-06-01",
	}
	if p, ok := provider.Lookup("claude"); ok {
		for k, v := range p.Identity {
			h[k] = v
		}
	}
	u := claudeUsageURL + "?at_wall=1&skip_spend=1"
	if over, ok := a.baseOverride["claude"]; ok {
		u = over + "/api/oauth/usage?at_wall=1&skip_spend=1"
	}
	var d map[string]any
	if _, err := fetchJSON(ctx, "GET", u, h, nil, &d); err != nil {
		return AccountQuota{}, err
	}
	q := AccountQuota{Windows: []QuotaWindow{}, Resets: []QuotaReset{}}
	add := func(name string, w map[string]any) {
		if w == nil {
			return
		}
		u, ok := jnum(w["utilization"])
		if !ok {
			return
		}
		q.Windows = append(q.Windows, QuotaWindow{Name: name, UsedPct: clampPct(u), ResetAt: resetTime(w["resets_at"])})
	}
	add("5h", jobj(d["five_hour"]))
	add("7d", jobj(d["seven_day"]))
	var keys []string
	for k := range d {
		if strings.HasPrefix(k, "seven_day_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		add("7d "+strings.TrimPrefix(k, "seven_day_"), jobj(d[k]))
	}
	q.HasJuniperTide = jobj(d["juniper_tide"]) != nil
	q.HasCedarEmber = jobj(d["cedar_ember"]) != nil
	q.Resets = claudeResetRows(d, time.Now().UTC())
	if q.Resets == nil {
		q.Resets = []QuotaReset{}
	}
	return q, nil
}

// quotaCopilot reads the Copilot plan's monthly pools.
func quotaCopilot(a *api, ctx context.Context, c store.Connection, token string) (AccountQuota, error) {
	var d map[string]any
	if _, err := fetchJSON(ctx, "GET", copilotUserURL, map[string]string{"Authorization": "token " + token,
		"X-Github-Api-Version": "2022-11-28", "User-Agent": "GitHubCopilotChat/" + provider.CopilotChatVersion,
		"Editor-Version": "vscode/" + provider.CopilotVSCodeVersion, "Editor-Plugin-Version": "copilot-chat/" + provider.CopilotChatVersion}, nil, &d); err != nil {
		return AccountQuota{}, err
	}
	q := AccountQuota{}
	q.Plan, _ = d["copilot_plan"].(string)
	reset := resetTime(d["quota_reset_date"])
	if reset == "" {
		if s, ok := d["quota_reset_date"].(string); ok {
			reset = s
		}
	}
	if snaps := jobj(d["quota_snapshots"]); snaps != nil {
		names := []string{}
		for k := range snaps {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			s := jobj(snaps[k])
			w := QuotaWindow{Name: k, UsedPct: -1, ResetAt: reset}
			if u, _ := s["unlimited"].(bool); u {
				w.Unlimited = true
			} else if ent, ok := jnum(s["entitlement"]); ok && ent > 0 {
				rem, _ := jnum(s["remaining"])
				w.UsedPct = clampPct(100 * (ent - rem) / ent)
				w.Used = strconv.FormatFloat(ent-rem, 'f', -1, 64)
				w.Limit = strconv.FormatFloat(ent, 'f', -1, 64)
			} else if pr, ok := jnum(s["percent_remaining"]); ok {
				w.UsedPct = clampPct(100 - pr)
			}
			q.Windows = append(q.Windows, w)
		}
		return q, nil
	}
	used, monthly := jobj(d["limited_user_quotas"]), jobj(d["monthly_quotas"])
	lreset := resetTime(d["limited_user_reset_date"])
	for _, k := range []string{"chat", "completions"} {
		total, ok := jnum(monthly[k])
		if !ok || total <= 0 {
			continue
		}
		rem, _ := jnum(used[k])
		// limited_user_quotas holds what is left of the month's pool.
		q.Windows = append(q.Windows, QuotaWindow{Name: k, UsedPct: clampPct(100 * (total - rem) / total),
			Used: strconv.FormatFloat(total-rem, 'f', -1, 64), Limit: strconv.FormatFloat(total, 'f', -1, 64), ResetAt: lreset})
	}
	if q.Plan == "" {
		q.Plan, _ = d["access_type_sku"].(string)
	}
	return q, nil
}

// quotaAntigravity reads the weekly buckets and each model's share.
func quotaAntigravity(a *api, ctx context.Context, c store.Connection, token string) (AccountQuota, error) {
	project, err := a.antigravityProject(ctx, c, token)
	if err != nil {
		return AccountQuota{}, err
	}
	h := map[string]string{"Authorization": "Bearer " + token, "User-Agent": provider.AntigravityUserAgent,
		"X-Client-Name": "antigravity", "X-Client-Version": provider.AntigravityVersion}
	base := antigravityQuotaURL
	if over, ok := a.baseOverride["antigravity"]; ok {
		base = over
	}
	q := AccountQuota{}
	var sum map[string]any
	if _, err := fetchJSON(ctx, "POST", base+"/v1internal:retrieveUserQuotaSummary", h, map[string]any{"project": project}, &sum); err == nil {
		groups, _ := sum["groups"].([]any)
		if groups == nil {
			groups, _ = jobj(sum["quotaSummary"])["groups"].([]any)
		}
		for _, g := range groups {
			gm := jobj(g)
			gname, _ := gm["displayName"].(string)
			buckets, _ := gm["buckets"].([]any)
			for _, b := range buckets {
				bm := jobj(b)
				if dis, _ := bm["disabled"].(bool); dis {
					continue
				}
				name, _ := bm["displayName"].(string)
				if name == "" {
					name, _ = bm["bucketId"].(string)
				}
				name = shortBucket(gname) + " · " + shortBucket(name)
				frac, ok := jnum(bm["remainingFraction"])
				w := QuotaWindow{Name: name, UsedPct: -1, ResetAt: resetTime(bm["resetTime"])}
				if ok {
					w.UsedPct = clampPct(100 * (1 - frac))
				}
				q.Windows = append(q.Windows, w)
			}
		}
	}
	var models map[string]any
	if _, err := fetchJSON(ctx, "POST", base+"/v1internal:fetchAvailableModels", h, map[string]any{"project": project}, &models); err != nil {
		if len(q.Windows) == 0 {
			return AccountQuota{}, err
		}
		return q, nil
	}
	ms := jobj(models["models"])
	names := []string{}
	for k := range ms {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		m := jobj(ms[k])
		if in, _ := m["isInternal"].(bool); in {
			continue
		}
		qi := jobj(m["quotaInfo"])
		if qi == nil {
			continue
		}
		frac, ok := jnum(qi["remainingFraction"])
		if !ok {
			frac = 0
		}
		q.Windows = append(q.Windows, QuotaWindow{Name: "model " + k, UsedPct: clampPct(100 * (1 - frac)), ResetAt: resetTime(qi["resetTime"])})
	}
	return q, nil
}

// quotaGroq reads the request and token limits from the headers of a cheap
// call; Groq has no quota endpoint.
func quotaGroq(a *api, ctx context.Context, c store.Connection, token string) (AccountQuota, error) {
	url := groqModelsURL
	if over, ok := a.baseOverride["groq"]; ok {
		url = over + "/models"
	}
	hdr, err := fetchJSON(ctx, "GET", url, map[string]string{"Authorization": "Bearer " + token}, nil, nil)
	if err != nil {
		return AccountQuota{}, err
	}
	got := map[string]string{}
	for k, v := range hdr {
		if len(v) > 0 {
			got[strings.ToLower(k)] = v[0]
		}
	}
	return AccountQuota{Windows: windowsFromHeaders(got)}, nil
}

// quotaOpenRouter reads the key's credit use and limit.
func quotaOpenRouter(a *api, ctx context.Context, c store.Connection, token string) (AccountQuota, error) {
	var d struct {
		Data map[string]any `json:"data"`
	}
	if _, err := fetchJSON(ctx, "GET", openrouterKeyURL, map[string]string{"Authorization": "Bearer " + token}, nil, &d); err != nil {
		return AccountQuota{}, err
	}
	q := AccountQuota{}
	if free, _ := d.Data["is_free_tier"].(bool); free {
		q.Plan = "free tier"
	}
	usage, _ := jnum(d.Data["usage"])
	w := QuotaWindow{Name: "credits", UsedPct: -1, Used: "$" + strconv.FormatFloat(usage, 'f', 2, 64)}
	if limit, ok := jnum(d.Data["limit"]); ok && limit > 0 {
		w.Limit = "$" + strconv.FormatFloat(limit, 'f', 2, 64)
		w.UsedPct = clampPct(100 * usage / limit)
	} else {
		w.Unlimited = true
	}
	q.Windows = append(q.Windows, w)
	return q, nil
}

// shortBucket trims Antigravity's bucket wording: "Gemini Models" → "Gemini",
// "Weekly Limit Remaining" → "weekly", "Five Hour Limit Remaining" → "5h".
func shortBucket(s string) string {
	r := strings.NewReplacer(" Limit Remaining", "", " Limit", "", " Remaining", "", " Models", "", " models", "",
		"Five Hour", "5h", "Weekly", "weekly", "Daily", "daily")
	return strings.TrimSpace(r.Replace(s))
}
