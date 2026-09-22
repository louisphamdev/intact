package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// Signing in an OAuth account from the dashboard. Each provider is reached as
// its own client registers it, so the redirect is the one that client uses:
// on a remote server the browser cannot reach it, and the person pastes the
// URL (or the code the page shows) back into intact. GitHub uses the device
// flow instead: intact shows a code, the person enters it at github.com.

type loginSpec struct {
	authorizeURL string
	tokenURL     string
	clientID     string
	redirectURI  string
	scope        string
	pkce         bool
	jsonExchange bool
	extra        map[string]string
}

var loginSpecs = map[string]loginSpec{
	"claude": {
		authorizeURL: "https://claude.ai/oauth/authorize",
		tokenURL:     "https://api.anthropic.com/v1/oauth/token",
		clientID:     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		redirectURI:  "https://console.anthropic.com/oauth/code/callback",
		scope:        "org:create_api_key user:profile user:inference",
		pkce:         true,
		jsonExchange: true,
		extra:        map[string]string{"code": "true"},
	},
	"codex": {
		authorizeURL: "https://auth.openai.com/oauth/authorize",
		tokenURL:     "https://auth.openai.com/oauth/token",
		clientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
		redirectURI:  "http://localhost:1455/auth/callback",
		scope:        "openid profile email offline_access",
		pkce:         true,
		extra: map[string]string{"id_token_add_organizations": "true", "codex_cli_simplified_flow": "true",
			"originator": "codex_cli_rs"},
	},
	"antigravity": {
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:     "https://oauth2.googleapis.com/token",
		clientID:     "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com",
		redirectURI:  "http://localhost:51121/oauth-callback",
		scope: "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email " +
			"https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog " +
			"https://www.googleapis.com/auth/experimentsandconfigs",
		extra: map[string]string{"access_type": "offline", "prompt": "consent"},
	},
}

// GitHub's device flow, as the Copilot Chat extension signs in.
var (
	githubDeviceURL = "https://github.com/login/device/code"
	githubTokenURL  = "https://github.com/login/oauth/access_token"
	githubUserURL   = "https://api.github.com/user"
	githubClientID  = "Iv1.b507a08c87ecfe98"
)

type pendingLogin struct {
	provider, verifier, state string
	at                        time.Time
}

type loginStore struct {
	mu sync.Mutex
	m  map[string]pendingLogin
}

func (l *loginStore) put(p pendingLogin) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range l.m {
		if time.Since(v.at) > 15*time.Minute {
			delete(l.m, k)
		}
	}
	l.m[p.state] = p
}

func (l *loginStore) take(state string) (pendingLogin, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.m[state]
	delete(l.m, state)
	if ok && time.Since(p.at) > 15*time.Minute {
		return pendingLogin{}, false
	}
	return p, ok
}

func randomURLSafe(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// loginStart begins a sign-in: the authorize URL for a redirect provider, or a
// device code for GitHub.
func (a *api) loginStart(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("provider")
	if prov == "github" {
		a.githubDeviceStart(w, r)
		return
	}
	spec, ok := loginSpecs[prov]
	if !ok {
		writeError(w, http.StatusNotFound, "this provider has no sign-in")
		return
	}
	p := pendingLogin{provider: prov, state: randomURLSafe(24), verifier: randomURLSafe(48), at: time.Now()}
	q := url.Values{"client_id": {spec.clientID}, "response_type": {"code"}, "redirect_uri": {spec.redirectURI},
		"scope": {spec.scope}, "state": {p.state}}
	if spec.pkce {
		sum := sha256.Sum256([]byte(p.verifier))
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range spec.extra {
		q.Set(k, v)
	}
	a.logins.put(p)
	writeJSON(w, map[string]any{"url": spec.authorizeURL + "?" + q.Encode(), "state": p.state, "redirect": spec.redirectURI})
}

// parseCallback reads the code and state from what the person pasted: the full
// redirect URL, "code#state" (Anthropic's page), or the bare code.
func parseCallback(input string) (code, state string) {
	input = strings.TrimSpace(input)
	if u, err := url.Parse(input); err == nil && u.RawQuery != "" && strings.Contains(u.RawQuery, "code=") {
		return u.Query().Get("code"), u.Query().Get("state")
	}
	if c, s, ok := strings.Cut(input, "#"); ok {
		return c, s
	}
	return input, ""
}

// loginFinish exchanges the pasted code for tokens and stores the account.
func (a *api) loginFinish(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
		Input string `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	code, state := parseCallback(body.Input)
	if state == "" {
		state = body.State
	}
	p, ok := a.logins.take(body.State)
	if !ok || p.provider != r.PathValue("provider") {
		writeError(w, http.StatusBadRequest, "this sign-in expired; start again")
		return
	}
	if state != p.state || code == "" {
		writeError(w, http.StatusBadRequest, "the pasted URL does not belong to this sign-in")
		return
	}
	c, err := a.exchangeLogin(r.Context(), p, code)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"connection": c})
}

type tokenAnswer struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"`
	Account      struct {
		Email string `json:"email_address"`
	} `json:"account"`
	Error     string `json:"error"`
	ErrorDesc string `json:"error_description"`
}

func (a *api) exchangeLogin(ctx context.Context, p pendingLogin, code string) (store.Connection, error) {
	spec := loginSpecs[p.provider]
	secret := ""
	if p.provider == "antigravity" {
		secret = a.antigravityClientSecret()
		if secret == "" {
			return store.Connection{}, errors.New("no Antigravity client secret: import one account first or set INTACT_ANTIGRAVITY_CLIENT_SECRET")
		}
	}
	fields := map[string]string{"grant_type": "authorization_code", "client_id": spec.clientID, "code": code,
		"redirect_uri": spec.redirectURI}
	if spec.pkce {
		fields["code_verifier"] = p.verifier
	}
	if spec.jsonExchange {
		fields["state"] = p.state
	}
	if secret != "" {
		fields["client_secret"] = secret
	}
	var req *http.Request
	var err error
	if spec.jsonExchange {
		b, _ := json.Marshal(fields)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, spec.tokenURL, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, spec.tokenURL, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return store.Connection{}, err
	}
	req.Header.Set("Accept", "application/json")
	t, err := doToken(req)
	if err != nil {
		return store.Connection{}, err
	}

	label, meta := p.provider, map[string]string{}
	switch p.provider {
	case "claude":
		if t.Account.Email != "" {
			label = t.Account.Email
		}
	case "codex":
		if email, acct := idTokenClaims(t.IDToken); email != "" {
			label = email
			meta["chatgptAccountId"] = acct
		}
	case "antigravity":
		if email := googleEmail(ctx, t.AccessToken); email != "" {
			label = email
		}
	}
	return a.saveOAuthAccount(p.provider, label, t, spec.tokenURL, spec.clientID, secret, meta)
}

func doToken(req *http.Request) (tokenAnswer, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tokenAnswer{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var t tokenAnswer
	json.Unmarshal(raw, &t)
	if resp.StatusCode >= 300 || t.AccessToken == "" {
		msg := t.ErrorDesc
		if msg == "" {
			msg = t.Error
		}
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return t, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, msg)
	}
	return t, nil
}

func (a *api) saveOAuthAccount(prov, label string, t tokenAnswer, tokenURL, clientID, secret string, meta map[string]string) (store.Connection, error) {
	c, err := a.store.CreateConnection(prov, label, t.AccessToken)
	if err != nil {
		return store.Connection{}, err
	}
	exp := ""
	if t.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	if t.RefreshToken != "" || tokenURL != "" {
		if err := a.store.SetOAuth(c.ID, store.OAuthCreds{RefreshToken: t.RefreshToken, TokenURL: tokenURL,
			ClientID: clientID, ClientSecret: secret, ExpiresAt: exp}); err != nil {
			return c, err
		}
	}
	if len(meta) > 0 {
		a.store.SetMeta(c.ID, meta)
	}
	return c, nil
}

// antigravityClientSecret is the Antigravity app's installed-client secret:
// taken from an imported account, or from the environment.
func (a *api) antigravityClientSecret() string {
	if s := os.Getenv("INTACT_ANTIGRAVITY_CLIENT_SECRET"); s != "" {
		return s
	}
	var s string
	a.store.DB.QueryRow(`SELECT client_secret FROM connections WHERE provider = 'antigravity' AND client_secret <> '' LIMIT 1`).Scan(&s)
	return s
}

// idTokenClaims reads the email and ChatGPT account id from an OpenAI id token.
func idTokenClaims(tok string) (email, account string) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var c struct {
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	json.Unmarshal(raw, &c)
	return c.Email, c.Auth.AccountID
}

func googleEmail(ctx context.Context, token string) string {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var u struct {
		Email string `json:"email"`
	}
	json.NewDecoder(resp.Body).Decode(&u)
	return u.Email
}

// githubDeviceStart asks GitHub for a device code.
func (a *api) githubDeviceStart(w http.ResponseWriter, r *http.Request) {
	form := url.Values{"client_id": {githubClientID}, "scope": {"read:user"}}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, githubDeviceURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "github unreachable")
		return
	}
	defer resp.Body.Close()
	var d struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Interval        int    `json:"interval"`
		ExpiresIn       int    `json:"expires_in"`
	}
	if json.NewDecoder(resp.Body).Decode(&d) != nil || d.DeviceCode == "" {
		writeError(w, http.StatusBadGateway, "github gave no device code")
		return
	}
	writeJSON(w, map[string]any{"device": true, "deviceCode": d.DeviceCode, "userCode": d.UserCode,
		"verificationUri": d.VerificationURI, "interval": d.Interval, "expiresIn": d.ExpiresIn})
}

// githubDevicePoll checks whether the person has entered the code yet.
func (a *api) githubDevicePoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode string `json:"deviceCode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.DeviceCode == "" {
		writeError(w, http.StatusBadRequest, "deviceCode is required")
		return
	}
	form := url.Values{"client_id": {githubClientID}, "device_code": {body.DeviceCode},
		"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, githubTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "github unreachable")
		return
	}
	defer resp.Body.Close()
	var t tokenAnswer
	json.NewDecoder(resp.Body).Decode(&t)
	switch {
	case t.AccessToken != "":
	case t.Error == "authorization_pending" || t.Error == "slow_down":
		writeJSON(w, map[string]any{"status": "pending"})
		return
	default:
		writeError(w, http.StatusBadRequest, "github: "+firstNonEmpty(t.ErrorDesc, t.Error, "sign-in failed"))
		return
	}
	label := "github"
	ureq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, githubUserURL, nil)
	ureq.Header.Set("Authorization", "token "+t.AccessToken)
	if uresp, err := http.DefaultClient.Do(ureq); err == nil {
		var u struct {
			Login string `json:"login"`
		}
		json.NewDecoder(uresp.Body).Decode(&u)
		uresp.Body.Close()
		if u.Login != "" {
			label = u.Login
		}
	}
	c, err := a.saveOAuthAccount("github", label, t, "", "", "", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot store the account")
		return
	}
	writeJSON(w, map[string]any{"status": "done", "connection": c})
}
