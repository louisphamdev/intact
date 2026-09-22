package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/provider"
	"github.com/louisphamdev/intact/internal/upstream"
)

// copilotTokenURL trades a GitHub OAuth token for a Copilot token. It is a
// variable so a test can point it at a fake.
var copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"

type copilotToken struct {
	token string
	exp   time.Time
}

// copilotCache holds the Copilot token of each connection until it is close
// to expiry, so the exchange runs about once every half hour, not per request.
type copilotCache struct {
	mu sync.Mutex
	m  map[string]copilotToken
}

// exchanged returns the bearer to send for a provider whose stored credential
// must be exchanged first, or the credential itself when it needs none.
func (a *api) exchanged(ctx context.Context, p provider.Provider, connID, secret string) (string, error) {
	if p.Exchange != "copilot" {
		return secret, nil
	}
	a.copilot.mu.Lock()
	t, ok := a.copilot.m[connID]
	a.copilot.mu.Unlock()
	if ok && time.Until(t.exp) > 5*time.Minute {
		return t.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+secret)
	req.Header.Set("User-Agent", "GitHubCopilotChat/"+provider.CopilotChatVersion)
	req.Header.Set("Editor-Version", "vscode/"+provider.CopilotVSCodeVersion)
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/"+provider.CopilotChatVersion)
	req.Header.Set("X-Github-Api-Version", provider.CopilotAPIVersion)
	req.Header.Set("Accept", "application/json")
	resp, err := upstream.Do(ctx, req, 2)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("copilot token exchange: status %d", resp.StatusCode)
	}
	var d struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &d); err != nil || d.Token == "" {
		return "", fmt.Errorf("copilot token exchange: no token in answer")
	}
	t = copilotToken{token: d.Token, exp: time.Unix(d.ExpiresAt, 0)}
	a.copilot.mu.Lock()
	a.copilot.m[connID] = t
	a.copilot.mu.Unlock()
	return t.token, nil
}

// dropExchanged forgets a connection's exchanged token after the upstream
// refused it, so the next call exchanges again.
func (a *api) dropExchanged(connID string) {
	a.copilot.mu.Lock()
	delete(a.copilot.m, connID)
	a.copilot.mu.Unlock()
}

// copilotTokenLimit matches the models that take max_completion_tokens and
// reject max_tokens on Copilot.
var copilotTokenLimit = regexp.MustCompile(`(?i)gpt-5|o[134]-`)

// adjustForProvider applies a provider's own request rules that a blacklist
// cannot express (a rename, not a removal).
func adjustForProvider(p provider.Provider, body []byte) []byte {
	if p.Exchange != "copilot" {
		return body
	}
	model, _ := bodyModel(body)
	if !copilotTokenLimit.MatchString(model) {
		return body
	}
	start, end, err := topLevelValue(body, "max_tokens")
	if err != nil {
		return body
	}
	if _, _, err := topLevelValue(body, "max_completion_tokens"); err == nil {
		return body
	}
	// Rename the key in place: find the quoted name just before the value.
	k := lastIndexBefore(body, start, `"max_tokens"`)
	if k < 0 {
		return body
	}
	_ = end
	out := append([]byte{}, body[:k]...)
	out = append(out, `"max_completion_tokens"`...)
	return append(out, body[k+len(`"max_tokens"`):]...)
}

func lastIndexBefore(b []byte, before int, s string) int {
	for i := before - len(s); i >= 0; i-- {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}

// secret0 re-reads a connection's stored credential for a second exchange.
func secret0(a *api, r *http.Request, connID string) string {
	s, _ := a.secretFor(r.Context(), connID)
	return s
}
