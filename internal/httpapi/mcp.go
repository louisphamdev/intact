package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"github.com/louisphamdev/intact/internal/filter"
	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
)

// intact serves the Model Context Protocol over Streamable HTTP at /mcp, so an
// agent can inspect and adjust it with the same API token a client uses. Each
// tool maps onto one of the management endpoints under /api.

const mcpProtocol = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name             string         `json:"name"`
	Description      string         `json:"description"`
	InputSchema      map[string]any `json:"inputSchema"`
	run              func(a *api, args map[string]any) (any, error)
	runWithPrincipal func(a *api, p principal, args map[string]any) (any, error)
}

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	pString = map[string]any{"type": "string"}
	pBool   = map[string]any{"type": "boolean"}
	pKind   = map[string]any{"type": "string", "enum": []string{filter.Field, filter.Schema, filter.System, filter.Header},
		"description": "field: dot path into the body (* matches any key or item); schema: key removed at every depth of tool schemas; system: regex of system-prompt lines to remove; header: request header not sent"}
	pProvider = map[string]any{"type": "string", "description": `provider id, or "*" for every provider`}
)

// mcpAdminTools are the tools a dashboard inference key may not run through
// /mcp: config changes, credential moves, reads of a credential, and reads of
// another client's stored bodies. Only the master token, or the open loopback
// server, may.
var mcpAdminTools = map[string]bool{
	"set_account_active": true, "set_models_active": true, "test_account": true,
	"set_model_policy": true,
	"put_provider_def": true, "delete_provider_def": true,
	"add_filter": true, "update_filter": true, "delete_filter": true,
	"ack_drift_changes": true, "drift_review": true, "error_review": true,
	"put_notify_channel": true, "test_notify_channel": true, "delete_notify_channel": true,
	"list_notify_channels": true, "get_error": true,
}

// mcpMask hides what a tool must not show a caller that is not admin. The
// reply of an admin stays as stored, so an edit-and-save round trip never
// writes a mask. A shape the mask does not know is dropped, never passed on.
var mcpMask = map[string]func(any) any{
	"list_provider_defs": func(v any) any {
		defs, ok := v.([]provider.Def)
		if !ok {
			return nil
		}
		out := make([]provider.Def, len(defs))
		for i, d := range defs {
			out[i] = maskDef(d)
		}
		return out
	},
	"list_drift_changes": func(v any) any {
		changes, ok := v.([]store.ShapeChange)
		if !ok {
			return nil
		}
		out := make([]store.ShapeChange, len(changes))
		for i, c := range changes {
			c.Sample = ""
			out[i] = c
		}
		return out
	},
}

var mcpTools = []mcpTool{
	{Name: "list_providers", Description: "List the providers intact can call and how many accounts each has.",
		InputSchema: schema(map[string]any{}),
		run:         func(a *api, _ map[string]any) (any, error) { return a.providerSummary(), nil }},
	{Name: "list_accounts", Description: "List stored accounts (never their credentials), optionally for one provider.",
		InputSchema: schema(map[string]any{"provider": pString}),
		run: func(a *api, args map[string]any) (any, error) {
			list, err := a.store.ListConnections()
			if p := argStr(args, "provider"); p != "" {
				out := []store.Connection{}
				for _, c := range list {
					if c.Provider == p {
						out = append(out, c)
					}
				}
				list = out
			}
			return list, err
		}},
	{Name: "set_account_active", Description: "Turn an account on or off. An inactive account is skipped by /v1.",
		InputSchema: schema(map[string]any{"id": pString, "active": pBool}, "id", "active"),
		run: func(a *api, args map[string]any) (any, error) {
			active, _ := args["active"].(bool)
			if err := a.store.SetActive(argStr(args, "id"), active); err != nil {
				return nil, err
			}
			return map[string]any{"id": argStr(args, "id"), "active": active}, nil
		}},
	{Name: "list_models", Description: "List the models callable through /v1 as <provider>/<model>, optionally for one provider.",
		InputSchema: schema(map[string]any{"provider": pString}),
		run: func(a *api, args map[string]any) (any, error) {
			out := []string{}
			for _, p := range a.activeProviders() {
				if want := argStr(args, "provider"); want != "" && want != p {
					continue
				}
				for _, m := range a.providerModelsBg(p) {
					out = append(out, p+"/"+m)
				}
			}
			return out, nil
		}},
	{Name: "list_provider_models", Description: "List a provider's models with their on/off switch and the last test.",
		InputSchema: schema(map[string]any{"provider": pString}, "provider"),
		run: func(a *api, args map[string]any) (any, error) {
			return a.modelRows(context.Background(), argStr(args, "provider"))
		}},
	{Name: "set_models_active", Description: "Switch models of a provider on or off. A model switched off is not listed in /v1/models nor served.",
		InputSchema: schema(map[string]any{"provider": pString, "models": map[string]any{"type": "array", "items": pString}, "active": pBool}, "provider", "models", "active"),
		run: func(a *api, args map[string]any) (any, error) {
			var ms []string
			for _, v := range list(args["models"]) {
				if s, ok := v.(string); ok {
					ms = append(ms, s)
				}
			}
			active, _ := args["active"].(bool)
			n, err := a.store.SetModelsActive(argStr(args, "provider"), ms, active)
			return map[string]any{"updated": n}, err
		}},
	{Name: "test_account", Description: "Test one account (connection id) with a short call, on model or on the provider's model that last passed. TypeSafe accounts get a System One test with known answers.",
		InputSchema: schema(map[string]any{"id": pString, "model": pString}, "id"),
		run: func(a *api, args map[string]any) (any, error) {
			res, _, err := a.runAccountTest(context.Background(), argStr(args, "id"), argStr(args, "model"))
			return res, err
		}},
	{Name: "get_model_rankings", Description: "How strong a provider's models are: each model's LMArena leaderboard entry (Elo-style rating from human votes, rank and tier S–D) on the overall, coding and webdev boards. Models not on the leaderboard are left out.",
		InputSchema: schema(map[string]any{"provider": pString}, "provider"),
		run: func(a *api, args map[string]any) (any, error) {
			prov := argStr(args, "provider")
			rows, err := a.modelRows(context.Background(), prov)
			if err != nil {
				return nil, err
			}
			ids := make([]string, len(rows))
			for i, r := range rows {
				ids[i] = r.Model
			}
			return map[string]any{"meta": a.arenaMeta(), "models": a.arenaFor(prov, ids)}, nil
		}},
	{Name: "set_model_policy", Description: "Set how a provider's models are switched on. autoTest: fetch the list, test every model and keep on only those that answer (re-run every 6 hours and for new models). onlyFree: only models whose name contains free are on, and only they are tested. Without autoTest, new models start on.",
		InputSchema: schema(map[string]any{"provider": pString, "autoTest": pBool, "onlyFree": pBool}, "provider", "autoTest", "onlyFree"),
		run: func(a *api, args map[string]any) (any, error) {
			at, _ := args["autoTest"].(bool)
			of, _ := args["onlyFree"].(bool)
			return a.updatePolicy(argStr(args, "provider"), at, of)
		}},
	{Name: "list_provider_defs", Description: "List the declared providers (added from the dashboard, the API or MCP rather than built in), with their definition. A dashboard key reads the credentials masked.",
		InputSchema: schema(map[string]any{}),
		run:         func(a *api, args map[string]any) (any, error) { return a.store.ProviderDefs() }},
	{Name: "put_provider_def", Description: "Declare a provider, or replace a declared provider's definition. def: {id, name, kind: apikey|oauth-code|oauth-device, api: openai|anthropic|responses, baseUrl, authHeader?, authPrefix?, headers?, modelsUrl? (or \"none\" with models), models?, color?, icon?, oauth?: {authorizeUrl|deviceCodeUrl, tokenUrl, clientId, clientSecret?, scope?, redirectUri?, verifyUrl?, noPkce?, jsonToken?, extra?}}.",
		InputSchema: schema(map[string]any{"def": map[string]any{"type": "object"}}, "def"),
		run: func(a *api, args map[string]any) (any, error) {
			raw, _ := json.Marshal(args["def"])
			var d provider.Def
			if err := json.Unmarshal(raw, &d); err != nil {
				return nil, err
			}
			return a.saveDef(d)
		}},
	{Name: "delete_provider_def", Description: "Delete a declared provider that has no account left.",
		InputSchema: schema(map[string]any{"id": pString}, "id"),
		run: func(a *api, args map[string]any) (any, error) {
			return map[string]any{"deleted": true}, a.removeDef(argStr(args, "id"))
		}},
	{Name: "list_filters", Description: "List the request filters (the blacklist), optionally for one provider scope.",
		InputSchema: schema(map[string]any{"provider": pProvider}),
		run: func(a *api, args map[string]any) (any, error) {
			list, err := a.store.ListFilters()
			if p := argStr(args, "provider"); p != "" {
				out := []store.Filter{}
				for _, f := range list {
					if f.Provider == p {
						out = append(out, f)
					}
				}
				list = out
			}
			return list, err
		}},
	{Name: "add_filter", Description: "Add a request filter. It applies to the next request, no restart.",
		InputSchema: schema(map[string]any{"provider": pProvider, "kind": pKind, "pattern": pString,
			"note": pString, "enabled": pBool}, "kind", "pattern"),
		run: func(a *api, args map[string]any) (any, error) {
			f := store.Filter{Provider: argStr(args, "provider"), Kind: argStr(args, "kind"),
				Pattern: argStr(args, "pattern"), Note: argStr(args, "note"), Enabled: true}
			if e, ok := args["enabled"].(bool); ok {
				f.Enabled = e
			}
			return a.putFilter(f)
		}},
	{Name: "update_filter", Description: "Change a filter; fields left out keep their value.",
		InputSchema: schema(map[string]any{"id": pString, "provider": pProvider, "kind": pKind, "pattern": pString,
			"note": pString, "enabled": pBool}, "id"),
		run: func(a *api, args map[string]any) (any, error) {
			list, err := a.store.ListFilters()
			if err != nil {
				return nil, err
			}
			for _, f := range list {
				if f.ID != argStr(args, "id") {
					continue
				}
				for k, dst := range map[string]*string{"provider": &f.Provider, "kind": &f.Kind, "pattern": &f.Pattern, "note": &f.Note} {
					if v, ok := args[k].(string); ok {
						*dst = v
					}
				}
				if e, ok := args["enabled"].(bool); ok {
					f.Enabled = e
				}
				return a.putFilter(f)
			}
			return nil, store.ErrFilterNotFound
		}},
	{Name: "delete_filter", Description: "Delete a request filter.",
		InputSchema: schema(map[string]any{"id": pString}, "id"),
		run: func(a *api, args map[string]any) (any, error) {
			if err := a.store.DeleteFilter(argStr(args, "id")); err != nil {
				return nil, err
			}
			a.reloadFilters()
			return map[string]any{"deleted": argStr(args, "id")}, nil
		}},
	{Name: "list_errors", Description: "List the errors providers answered, newest first: every failed attempt, also the ones intact failed over from. Class is network, timeout, auth, rejected, rate_limit, fake_rate_limit (a 429 answered at once while quota was left: the provider refused the content) or server. Bodies are left out; get_error has them.",
		InputSchema: schema(map[string]any{"provider": pString, "class": pString, "signature": pString, "status": map[string]any{"type": "integer"},
			"since": map[string]any{"type": "string", "description": "RFC 3339 time"}, "limit": map[string]any{"type": "integer"}}),
		run: func(a *api, args map[string]any) (any, error) {
			f := store.ErrorFilter{Provider: argStr(args, "provider"), Class: argStr(args, "class"), Signature: argStr(args, "signature"), Since: argStr(args, "since")}
			if v, ok := args["status"].(float64); ok {
				f.Status = int(v)
			}
			if v, ok := args["limit"].(float64); ok {
				f.Limit = int(v)
			}
			return a.store.ListUpstreamErrors(f)
		}},
	{Name: "get_error", Description: "One provider error with its headers, the answer's body and the request that was sent.",
		InputSchema: schema(map[string]any{"id": map[string]any{"type": "integer"}}, "id"),
		run: func(a *api, args map[string]any) (any, error) {
			id, _ := args["id"].(float64)
			return a.store.GetUpstreamError(int64(id))
		}},
	{Name: "error_stats", Description: "Provider errors grouped by signature (status and message with ids and numbers removed): count, classes, first and last time, models, median latency. Start an analysis here.",
		InputSchema: schema(map[string]any{"provider": pString, "since": map[string]any{"type": "string", "description": "RFC 3339 time"}}),
		run: func(a *api, args map[string]any) (any, error) {
			return a.errorGroups(store.ErrorFilter{Provider: argStr(args, "provider"), Since: argStr(args, "since")})
		}},
	{Name: "list_drift_changes", Description: "List structure changes seen in traffic: fields a client started or stopped sending (direction request), or a provider started or stopped answering (direction response). Use it to decide what to blacklist.",
		InputSchema: schema(map[string]any{"provider": pString, "direction": map[string]any{"type": "string", "enum": []string{"request", "response"}},
			"unacked": pBool, "since": map[string]any{"type": "integer"}, "limit": map[string]any{"type": "integer"}}),
		run: func(a *api, args map[string]any) (any, error) {
			f := store.ShapeChangeFilter{Provider: argStr(args, "provider"), Direction: argStr(args, "direction")}
			f.Unacked, _ = args["unacked"].(bool)
			if v, ok := args["since"].(float64); ok {
				f.SinceID = int64(v)
			}
			if v, ok := args["limit"].(float64); ok {
				f.Limit = int(v)
			}
			return a.store.ListShapeChanges(f)
		}},
	{Name: "ack_drift_changes", Description: "Mark structure changes as reviewed: the ids given, or all of them when ids is empty.",
		InputSchema: schema(map[string]any{"ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}}),
		run: func(a *api, args map[string]any) (any, error) {
			// An id that is not an integer must be an error: dropping it would
			// leave an empty list, which acknowledges every change.
			var ids []int64
			if raw, given := args["ids"]; given && raw != nil {
				l, ok := raw.([]any)
				if !ok {
					return nil, fmt.Errorf("ids must be a list of integers")
				}
				for _, v := range l {
					f, ok := v.(float64)
					if !ok || f != math.Trunc(f) {
						return nil, fmt.Errorf("ids must be a list of integers, got %v", v)
					}
					ids = append(ids, int64(f))
				}
			}
			n, err := a.store.AckShapeChanges(ids)
			return map[string]any{"acked": n}, err
		}},
	{Name: "error_review", Description: "The error review: a chat model (such as antigravity/gemini-3.8-flash) reads each recurring group of refused requests, fake 429s and sign-in errors, and proposes blacklisting what the provider refuses, switching off a model the provider dropped or an account whose sign-in is revoked, or leaving it. intact replays the failing request to check a rule before keeping it. Without arguments, its state. enabled needs a model. run judges the groups due now.",
		InputSchema: schema(map[string]any{"enabled": pBool, "model": pString, "minErrors": map[string]any{"type": "integer"}, "replay": pBool, "run": pBool}),
		run: func(a *api, args map[string]any) (any, error) {
			if v, ok := args["minErrors"].(float64); ok {
				args["minErrors"] = int(v)
			}
			return callHandler(a.errorReview, http.MethodPost, nil, args)
		}},
	{Name: "list_error_verdicts", Description: "The error review's verdicts, newest first: the group (provider and signature), the action, whether it was applied and checked by a replay, the rule, model or account acted on, and why.",
		InputSchema: schema(map[string]any{}),
		run: func(a *api, args map[string]any) (any, error) {
			return callHandler(a.errorVerdicts, http.MethodGet, nil, nil)
		}},
	{Name: "list_notify_channels", Description: "The alert channels (secrets masked), the channel types with their fields, and the events a channel can take.",
		InputSchema: schema(map[string]any{}),
		run: func(a *api, args map[string]any) (any, error) {
			return callHandler(a.notifyInfo, http.MethodGet, nil, nil)
		}},
	{Name: "put_notify_channel", Description: "Create an alert channel, or replace one with its id. type is telegram (config botToken, chatId, threadId for a forum topic, link) or webhook (config url, secret, link). events: the events it takes, empty for all. A secret left empty keeps the stored one.",
		InputSchema: schema(map[string]any{"id": pString, "name": pString, "type": pString, "enabled": pBool,
			"events": map[string]any{"type": "array", "items": pString}, "config": map[string]any{"type": "object"}}, "type"),
		run: func(a *api, args map[string]any) (any, error) {
			return callHandler(a.putChannel, http.MethodPost, map[string]string{"id": argStr(args, "id")}, args)
		}},
	{Name: "test_notify_channel", Description: "Send a test alert to one channel.",
		InputSchema: schema(map[string]any{"id": pString}, "id"),
		run: func(a *api, args map[string]any) (any, error) {
			return callHandler(a.testChannel, http.MethodPost, map[string]string{"id": argStr(args, "id")}, nil)
		}},
	{Name: "delete_notify_channel", Description: "Delete an alert channel.",
		InputSchema: schema(map[string]any{"id": pString}, "id"),
		run: func(a *api, args map[string]any) (any, error) {
			return callHandler(a.deleteChannel, http.MethodDelete, map[string]string{"id": argStr(args, "id")}, nil)
		}},
	{Name: "drift_review", Description: "The drift review: a System One decision model (such as typesafe/jev-latest) judges each structure change's cause and acknowledges the benign ones it is sure of; a resolver chat model (such as antigravity/gemini-3.8-flash), when set, settles every other change on its own: acknowledge, or blacklist a request field a provider refuses. Without arguments, its state. enabled needs a decisionModel. run judges the waiting changes now.",
		InputSchema: schema(map[string]any{"enabled": pBool, "decisionModel": pString, "resolverModel": pString, "ackConfidence": map[string]any{"type": "number"}, "run": pBool}),
		run: func(a *api, args map[string]any) (any, error) {
			var en *bool
			if v, ok := args["enabled"].(bool); ok {
				en = &v
			}
			var dm, rm *string
			if v, ok := args["decisionModel"].(string); ok {
				dm = &v
			}
			if v, ok := args["resolverModel"].(string); ok {
				rm = &v
			}
			var ack *float64
			if v, ok := args["ackConfidence"].(float64); ok {
				ack = &v
			}
			if en != nil || dm != nil || rm != nil || ack != nil {
				if _, err := a.updateReviewConfig(en, dm, rm, ack); err != nil {
					return nil, err
				}
			}
			out := a.reviewStatus()
			if run, _ := args["run"].(bool); run {
				n, err := a.reviewPending(context.Background())
				if err != nil {
					return nil, err
				}
				out["judged"] = n
			}
			return out, nil
		}},
	{Name: "list_drift_fields", Description: "List the field paths learned for a provider and direction, with their type and how often they were seen.",
		InputSchema: schema(map[string]any{"provider": pString, "direction": map[string]any{"type": "string", "enum": []string{"request", "response"}}, "endpoint": pString}),
		run: func(a *api, args map[string]any) (any, error) {
			return a.drift.Fields(argStr(args, "direction"), argStr(args, "provider"), argStr(args, "endpoint")), nil
		}},
	{Name: "get_quota", Description: "Read each active account's quota (rolling windows, resets, weekly and monthly pools, per-model shares) from the provider, or from the rate-limit headers of its last answer. The refresh parameter is ignored.",
		InputSchema: schema(map[string]any{"provider": pString, "refresh": pBool}),
		run: func(a *api, args map[string]any) (any, error) {
			conns, err := a.store.ListConnections()
			if err != nil {
				return nil, err
			}
			out := []AccountQuota{}
			for _, c := range conns {
				if _, ok := a.providerFor(c); ok && c.IsActive && (argStr(args, "provider") == "" || c.Provider == argStr(args, "provider")) {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					out = append(out, a.quotaFor(ctx, c, false))
					cancel()
				}
			}
			return out, nil
		}},
	{Name: "get_usage", Description: "Daily token totals per account and model, optionally for one day (YYYY-MM-DD).",
		InputSchema: schema(map[string]any{"day": pString}),
		run: func(a *api, args map[string]any) (any, error) {
			rows, err := a.store.Usage()
			if d := argStr(args, "day"); d != "" {
				out := []store.UsageRow{}
				for _, r := range rows {
					if r.Day == d {
						out = append(out, r)
					}
				}
				rows = out
			}
			return rows, err
		}},
	{Name: "list_contracts", Description: "List model and tool contract indexes with contract version hash, last sample, and open finding counts.",
		InputSchema: schema(map[string]any{"model": pString}),
		run: func(a *api, args map[string]any) (any, error) {
			return a.contractsIndexData(argStr(args, "model")), nil
		}},
	{Name: "get_contract", Description: "Get the learned contract of a model and the golden contract of each tool. Never returns hash or string lengths.",
		InputSchema: schema(map[string]any{"model": pString}, "model"),
		run: func(a *api, args map[string]any) (any, error) {
			m := argStr(args, "model")
			if m == "" {
				return nil, errors.New("model is required")
			}
			return a.contractModelDetailsData(m)
		}},
	{Name: "list_contract_findings", Description: "List contract discrepancies and findings with evidence. Trusted callers only.",
		InputSchema: schema(map[string]any{"model": pString, "status": pString}),
		runWithPrincipal: func(a *api, p principal, args map[string]any) (any, error) {
			if !a.isCallerTrusted(p) {
				return nil, errors.New("unauthorized: caller is not trusted to read contract findings")
			}
			status := argStr(args, "status")
			findings, err := a.store.ListContractFindings(status, 0, true)
			if err != nil {
				return nil, err
			}
			m := argStr(args, "model")
			if m != "" {
				var filtered []store.ContractFinding
				for _, f := range findings {
					if f.Model == m {
						filtered = append(filtered, f)
					}
				}
				findings = filtered
			}
			return a.formatFindingsJSON(findings), nil
		}},
}

func argStr(args map[string]any, k string) string {
	s, _ := args[k].(string)
	return strings.TrimSpace(s)
}

// putFilter validates and stores a filter, then applies it to the next request.
func (a *api) putFilter(f store.Filter) (store.Filter, error) {
	rule, err := filter.Compile(f.Kind, f.Pattern)
	if err != nil {
		return store.Filter{}, err
	}
	// Shared by the HTTP and the MCP entry points: neither may install a rule
	// that strips an essential field from every request.
	if why := unsafeFilter(f.Kind, rule.Pattern); why != "" {
		return store.Filter{}, errors.New(why)
	}
	f.Pattern = rule.Pattern
	saved, err := a.store.SaveFilter(f)
	if err != nil {
		return store.Filter{}, err
	}
	a.reloadFilters()
	return saved, nil
}

// providerModelsBg reads a provider's cached model list outside a request.
func (a *api) providerModelsBg(p string) []string {
	return a.providerModels(context.Background(), p)
}

// providerSummary is the provider list the management API and MCP return.
func (a *api) providerSummary() []map[string]any {
	list, _ := a.store.ListConnections()
	count, active := map[string]int{}, map[string]int{}
	usable := map[string]bool{}
	for _, c := range list {
		count[c.Provider]++
		if c.IsActive {
			active[c.Provider]++
		}
		if _, ok := a.providerFor(c); ok {
			usable[c.Provider] = true
		}
	}
	ids := map[string]bool{}
	for p := range count {
		ids[p] = true
	}
	for _, id := range provider.IDs() {
		ids[id] = true
		usable[id] = usable[id] || count[id] == 0
	}
	out := []map[string]any{}
	for p := range ids {
		out = append(out, map[string]any{"id": p, "accounts": count[p], "active": active[p], "supported": usable[p]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out
}

// mcp answers one JSON-RPC message of the Streamable HTTP transport.
func (a *api) mcp(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		rpcReply(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	// A notification has no id and gets no answer.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := mcpProtocol
		if p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
		rpcReply(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "intact", "version": "1"},
			"instructions":    "Manage intact: accounts, models, usage, and the request filters (blacklist) applied before a request reaches a provider.",
		}, nil)
	case "ping":
		rpcReply(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		rpcReply(w, req.ID, map[string]any{"tools": mcpTools}, nil)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			rpcReply(w, req.ID, nil, &rpcError{Code: -32602, Message: "invalid params"})
			return
		}
		// The dashboard key that reaches /mcp is a machine's inference key, not
		// the operator. A tool that changes a provider's URL, installs a filter,
		// moves a credential, or reads another client's stored bodies needs the
		// master token (or the open loopback server), the same bar the /api
		// routes hold. callHandler runs a handler with no request identity, so
		// the gate has to live here.
		admin := principalOf(r).admin
		for _, t := range mcpTools {
			if t.Name != p.Name {
				continue
			}
			if mcpAdminTools[t.Name] && !admin {
				rpcReply(w, req.ID, toolResult(nil, errors.New("this tool needs the master token, not a dashboard key")), nil)
				return
			}
			if p.Arguments == nil {
				p.Arguments = map[string]any{}
			}
			var res any
			var err error
			if t.runWithPrincipal != nil {
				res, err = t.runWithPrincipal(a, principalOf(r), p.Arguments)
			} else {
				res, err = t.run(a, p.Arguments)
			}
			if m := mcpMask[t.Name]; m != nil && !admin {
				res = m(res)
			}
			rpcReply(w, req.ID, toolResult(res, err), nil)
			return
		}
		rpcReply(w, req.ID, nil, &rpcError{Code: -32602, Message: "unknown tool " + p.Name})
	default:
		rpcReply(w, req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

// toolResult wraps a tool's value as MCP content; an error is reported to the
// model as a tool error, not a protocol error, so it can correct itself.
func toolResult(v any, err error) map[string]any {
	if err != nil {
		msg := err.Error()
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrFilterNotFound) {
			msg = "not found"
		}
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": msg}}}
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
}

func rpcReply(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		msg["id"] = nil
	}
	if e != nil {
		msg["error"] = e
	} else {
		msg["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msg)
}

// mcpGet refuses the server-to-client stream, which intact does not offer.
func (a *api) mcpGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("POST JSON-RPC messages to %s", r.URL.Path))
}

func list(v any) []any { l, _ := v.([]any); return l }

// callHandler runs a dashboard handler for an MCP tool and returns its JSON,
// or its error message as an error.
func callHandler(h http.HandlerFunc, method string, vals map[string]string, body any) (any, error) {
	raw, _ := json.Marshal(body)
	if body == nil {
		raw = nil
	}
	r := httptest.NewRequest(method, "/", bytes.NewReader(raw))
	for k, v := range vals {
		r.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	var out any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code >= 400 {
		if m, ok := out.(map[string]any); ok {
			if e, ok := m["error"].(string); ok {
				return nil, errors.New(e)
			}
		}
		return nil, fmt.Errorf("status %d", rec.Code)
	}
	return out, nil
}
