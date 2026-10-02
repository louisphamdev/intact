package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/upstream"
)

// antigravityMetadata is what the Antigravity CLI sends to loadCodeAssist, on the
// same daily host that serves its chat.
var antigravityMetadata = map[string]any{"ideType": "ANTIGRAVITY"}

// sigStore remembers the thought signature of each function call for an hour,
// so a follow-up turn can hand it back with the call.
type sigStore struct {
	mu sync.Mutex
	m  map[string]sigEntry
}

type sigEntry struct {
	sig string
	at  time.Time
}

func (s *sigStore) Get(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[id]; ok && time.Since(e.at) < time.Hour {
		return e.sig
	}
	return ""
}

func (s *sigStore) Put(id, sig string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) > 10000 {
		for k, e := range s.m {
			if time.Since(e.at) > time.Hour {
				delete(s.m, k)
			}
		}
	}
	s.m[id] = sigEntry{sig: sig, at: time.Now()}
}

// antigravityProject returns the Cloud project of an account, asking Cloud
// Code Assist once and keeping the answer in the connection's metadata.
func (a *api) antigravityProject(ctx context.Context, conn store.Connection, token string) (string, error) {
	if p := conn.Meta["projectId"]; p != "" {
		return p, nil
	}
	body, _ := json.Marshal(map[string]any{"metadata": antigravityMetadata})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.antigravityBase(conn)+"/v1internal:loadCodeAssist", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", provider.AntigravityUserAgent)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := upstream.Do(ctx, req, 2)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("loadCodeAssist: status %d", resp.StatusCode)
	}
	var d struct {
		Project json.RawMessage `json:"cloudaicompanionProject"`
	}
	json.Unmarshal(raw, &d)
	var id string
	if json.Unmarshal(d.Project, &id) != nil || id == "" {
		var o struct{ ID string }
		json.Unmarshal(d.Project, &o)
		id = o.ID
	}
	if id == "" {
		return "", fmt.Errorf("loadCodeAssist: no project for this account")
	}
	a.store.SetMeta(conn.ID, map[string]string{"projectId": id})
	return id, nil
}

// antigravityBase is the Code Assist host of a connection.
func (a *api) antigravityBase(conn store.Connection) string {
	if over, ok := a.baseOverride[conn.Provider]; ok {
		return over
	}
	p, _ := a.providerFor(conn)
	return p.BaseURL
}

// agModel is what fetchAvailableModels says about one model. The CLI copies
// these values into every request, so intact reads them instead of guessing.
type agModel struct {
	SupportsThinking bool   `json:"supportsThinking"`
	ThinkingBudget   int    `json:"thinkingBudget"`
	MaxOutputTokens  int    `json:"maxOutputTokens"`
	Enum             string `json:"model"`
	Provider         string `json:"modelProvider"`
}

// antigravityModelInfo looks a model up in the cached list of its provider.
func (a *api) antigravityModelInfo(prov, model string) (agModel, bool) {
	a.cat.mu.Lock()
	raw := a.cat.m[prov].raw
	a.cat.mu.Unlock()
	var d struct {
		Models map[string]agModel `json:"models"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return agModel{}, false
	}
	m, ok := d.Models[model]
	return m, ok
}

// agContent is one turn; the struct keeps role before parts, as the CLI writes it.
type agContent struct {
	Role  string            `json:"role"`
	Parts []json.RawMessage `json:"parts"`
}

// agRequest and agEnvelope fix the key order the CLI sends.
type agRequest struct {
	Contents          []agContent       `json:"contents"`
	SystemInstruction json.RawMessage   `json:"systemInstruction,omitempty"`
	Tools             json.RawMessage   `json:"tools,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	GenerationConfig  json.RawMessage   `json:"generationConfig,omitempty"`
	ToolConfig        json.RawMessage   `json:"toolConfig,omitempty"`
	SessionID         string            `json:"sessionId"`
}

type agEnvelope struct {
	Project     string    `json:"project"`
	RequestID   string    `json:"requestId"`
	Request     agRequest `json:"request"`
	Model       string    `json:"model"`
	UserAgent   string    `json:"userAgent"`
	RequestType string    `json:"requestType"`
}

// antigravityEnvelope wraps an inner Gemini request for Cloud Code Assist in
// the shape the Antigravity CLI sends.
func (a *api) antigravityEnvelope(ctx context.Context, conn store.Connection, token, model string, inner []byte) ([]byte, error) {
	project, err := a.antigravityProject(ctx, conn, token)
	if err != nil {
		return nil, err
	}
	var in struct {
		Contents          []agContent     `json:"contents"`
		SystemInstruction json.RawMessage `json:"systemInstruction"`
		Tools             []any           `json:"tools"`
		GenerationConfig  map[string]any  `json:"generationConfig"`
		ToolConfig        json.RawMessage `json:"toolConfig"`
	}
	d := json.NewDecoder(bytes.NewReader(inner))
	d.UseNumber()
	if err := d.Decode(&in); err != nil {
		return nil, err
	}
	info, known := a.antigravityModelInfo(conn.Provider, model)
	contents := in.Contents
	if info.Provider == "MODEL_PROVIDER_GOOGLE" {
		contents = splitFunctionResponses(contents)
	}
	req := agRequest{Contents: contents, SystemInstruction: in.SystemInstruction, ToolConfig: in.ToolConfig}
	if len(in.Tools) > 0 {
		for _, t := range in.Tools {
			for _, fd := range asList(asMap(t)["functionDeclarations"]) {
				upperSchemaTypes(asMap(asMap(fd)["parameters"]))
			}
		}
		req.Tools, _ = json.Marshal(in.Tools)
	}
	gen := in.GenerationConfig
	if known {
		if gen == nil {
			gen = map[string]any{}
		}
		if info.MaxOutputTokens > 0 {
			gen["maxOutputTokens"] = info.MaxOutputTokens
		}
		delete(gen, "thinkingConfig")
		if info.SupportsThinking {
			gen["thinkingConfig"] = map[string]any{"includeThoughts": true, "thinkingBudget": info.ThinkingBudget}
		}
	}
	if len(gen) > 0 {
		req.GenerationConfig, _ = json.Marshal(gen)
	}
	// The upstream cache follows the account, not this id: one stable id per
	// account only makes the account look like one client.
	h := sha256.Sum256([]byte("antigravity:" + conn.ID))
	req.SessionID = "-" + strconv.FormatUint(binary.BigEndian.Uint64(h[:8])&0x7fffffffffffffff, 10)
	traj := agTrajectory(contents)
	if known {
		claude := strconv.FormatBool(info.Provider == "MODEL_PROVIDER_ANTHROPIC")
		req.Labels = map[string]string{
			"last_step_index":          strconv.Itoa(len(contents) - 1),
			"model_enum":               info.Enum,
			"request_id":               traj + "-" + strconv.Itoa(agModelTurns(contents)),
			"trajectory_id":            traj,
			"used_claude":              claude,
			"used_claude_conservative": claude,
			"used_non_gemini_model":    strconv.FormatBool(info.Provider != "MODEL_PROVIDER_GOOGLE"),
		}
	}
	return json.Marshal(agEnvelope{
		Project:     project,
		RequestID:   fmt.Sprintf("agent/%s/%d/%s/%d", stableUUID("antigravity-session:"+conn.ID), time.Now().UnixMilli(), traj, len(contents)),
		Request:     req,
		Model:       model,
		UserAgent:   "antigravity",
		RequestType: "agent",
	})
}

// splitFunctionResponses moves tool results into turns of their own with the
// model role, as the CLI does for a Gemini model.
func splitFunctionResponses(in []agContent) []agContent {
	out := make([]agContent, 0, len(in))
	for _, c := range in {
		if c.Role != "user" {
			out = append(out, c)
			continue
		}
		var cur *agContent
		for _, p := range c.Parts {
			role := "user"
			if bytes.HasPrefix(bytes.TrimSpace(p), []byte(`{"functionResponse"`)) {
				role = "model"
			}
			if cur == nil || cur.Role != role {
				out = append(out, agContent{Role: role})
				cur = &out[len(out)-1]
			}
			cur.Parts = append(cur.Parts, p)
		}
	}
	return out
}

// agModelTurns counts the model turns that are not tool results.
func agModelTurns(contents []agContent) int {
	n := 0
	for _, c := range contents {
		if c.Role == "model" && len(c.Parts) > 0 && !bytes.HasPrefix(bytes.TrimSpace(c.Parts[0]), []byte(`{"functionResponse"`)) {
			n++
		}
	}
	return n
}

// agTrajectory names a conversation by its first turn, so every step of it
// carries the same trajectory, as one CLI session does.
func agTrajectory(contents []agContent) string {
	if len(contents) == 0 {
		return stableUUID("antigravity-trajectory:")
	}
	first, _ := json.Marshal(contents[0])
	return stableUUID("antigravity-trajectory:" + string(first))
}

// stableUUID formats a hash of seed as a version 4 UUID.
func stableUUID(seed string) string {
	b := sha256.Sum256([]byte(seed))
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// upperSchemaTypes writes schema types in upper case, as the CLI does.
func upperSchemaTypes(s map[string]any) {
	if s == nil {
		return
	}
	if t, ok := s["type"].(string); ok {
		s["type"] = strings.ToUpper(t)
	}
	for _, v := range asMap(s["properties"]) {
		upperSchemaTypes(asMap(v))
	}
	upperSchemaTypes(asMap(s["items"]))
	for _, k := range []string{"anyOf", "oneOf", "allOf"} {
		for _, v := range asList(s[k]) {
			upperSchemaTypes(asMap(v))
		}
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// antigravityModels lists an account's models with fetchAvailableModels.
func (a *api) antigravityModels(ctx context.Context, conn store.Connection) ([]string, []byte, bool) {
	token, err := a.secretFor(ctx, conn.ID)
	if err != nil {
		return nil, nil, false
	}
	project, _ := a.antigravityProject(ctx, conn, token)
	body, _ := json.Marshal(map[string]any{"project": project})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.antigravityBase(conn)+"/v1internal:fetchAvailableModels", bytes.NewReader(body))
	if err != nil {
		return nil, nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", provider.AntigravityUserAgent)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := upstream.Do(ctx, req, 2)
	if err != nil {
		return nil, nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, false
	}
	var d struct {
		Models map[string]struct {
			IsInternal bool `json:"isInternal"`
		} `json:"models"`
		// Deprecated models stay listed but refuse calls (400); the value
		// names the replacement, which is listed on its own.
		Deprecated map[string]json.RawMessage `json:"deprecatedModelIds"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || json.Unmarshal(raw, &d) != nil {
		return nil, nil, false
	}
	ids := []string{}
	for id, m := range d.Models {
		if _, gone := d.Deprecated[id]; !m.IsInternal && !gone {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, raw, true
}
