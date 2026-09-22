package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/store"
	"github.com/louisphamdev/intact/internal/upstream"
)

// Cloud Code Assist provisions the account's project on the production host;
// the daily host that serves chat refuses these calls.
var antigravityProdURL = "https://cloudcode-pa.googleapis.com"

// antigravityMetadata identifies the client as the Antigravity IDE.
var antigravityMetadata = map[string]any{"ideType": 9, "platform": 2, "pluginType": 2}

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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, antigravityProdURL+"/v1internal:loadCodeAssist", bytes.NewReader(body))
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

// antigravityEnvelope wraps an inner Gemini request for Cloud Code Assist.
func (a *api) antigravityEnvelope(ctx context.Context, conn store.Connection, token, model string, inner []byte) ([]byte, error) {
	project, err := a.antigravityProject(ctx, conn, token)
	if err != nil {
		return nil, err
	}
	var req map[string]any
	d := json.NewDecoder(bytes.NewReader(inner))
	d.UseNumber()
	if err := d.Decode(&req); err != nil {
		return nil, err
	}
	// A stable session per account lets the upstream reuse its cache.
	h := sha256.Sum256([]byte("antigravity:" + conn.ID))
	req["sessionId"] = "-" + strconv.FormatUint(binary.BigEndian.Uint64(h[:8])&0x7fffffffffffffff, 10)
	contents, _ := req["contents"].([]any)
	step := 2*len(contents) - 1
	if step < 1 {
		step = 1
	}
	env := map[string]any{
		"project":     project,
		"model":       model,
		"userAgent":   "antigravity",
		"requestType": "agent",
		"requestId":   fmt.Sprintf("agent/%s/%d/%s/%d", uuid4(), time.Now().UnixMilli(), uuid4(), step),
		"request":     req,
	}
	return json.Marshal(env)
}

func uuid4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// antigravityModels lists an account's models with fetchAvailableModels.
func (a *api) antigravityModels(ctx context.Context, conn store.Connection) ([]string, []byte, bool) {
	token, err := a.secretFor(ctx, conn.ID)
	if err != nil {
		return nil, nil, false
	}
	project, _ := a.antigravityProject(ctx, conn, token)
	p, _ := a.providerFor(conn)
	base := p.BaseURL
	if over, ok := a.baseOverride[conn.Provider]; ok {
		base = over
	}
	body, _ := json.Marshal(map[string]any{"project": project})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1internal:fetchAvailableModels", bytes.NewReader(body))
	if err != nil {
		return nil, nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", provider.AntigravityUserAgent)
	req.Header.Set("X-Client-Name", "antigravity")
	req.Header.Set("X-Client-Version", provider.AntigravityVersion)
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
