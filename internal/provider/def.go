package provider

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// A Def declares a provider from the dashboard, the API or MCP, without code:
// everything intact needs to call it, list its models and add its accounts.
// Kind decides which fields matter:
//
//	apikey        an account is an API key (or token) pasted in
//	oauth-code    an account signs in in the browser; the person pastes back
//	              the redirect URL or the code (authorization code + PKCE)
//	oauth-device  an account signs in with a device code typed on the
//	              provider's page (RFC 8628)
type Def struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Color string `json:"color,omitempty"`
	// Icon is an https: or data: image URL.
	Icon string `json:"icon,omitempty"`
	Kind string `json:"kind"`
	// API is the request shape: openai (Chat Completions), anthropic
	// (Messages) or responses (OpenAI Responses).
	API     string `json:"api"`
	BaseURL string `json:"baseUrl"`
	// AuthHeader and AuthPrefix carry the credential. Empty: Authorization /
	// "Bearer ", or X-Api-Key / "" for an anthropic API key.
	AuthHeader string `json:"authHeader,omitempty"`
	AuthPrefix string `json:"authPrefix,omitempty"`
	// Headers are sent on every call (for example anthropic-version).
	Headers map[string]string `json:"headers,omitempty"`
	// ModelsURL is where the model list is read: empty means BaseURL +
	// "/models"; "none" means the provider has no list (use Models).
	ModelsURL string `json:"modelsUrl,omitempty"`
	// Models stands in when the list cannot be read.
	Models []string `json:"models,omitempty"`
	OAuth  *OAuth   `json:"oauth,omitempty"`
	// Builtin marks a def as fixed (not stored, not editable).
	Builtin bool `json:"builtin,omitempty"`
}

// OAuth is how an account signs in and refreshes.
type OAuth struct {
	AuthorizeURL  string `json:"authorizeUrl,omitempty"`  // oauth-code
	DeviceCodeURL string `json:"deviceCodeUrl,omitempty"` // oauth-device
	// VerifyURL is the page the person opens with the device code, when the
	// device answer does not name one.
	VerifyURL    string `json:"verifyUrl,omitempty"`
	TokenURL     string `json:"tokenUrl"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
	Scope        string `json:"scope,omitempty"`
	RedirectURI  string `json:"redirectUri,omitempty"` // oauth-code
	// NoPKCE turns off PKCE for a provider that refuses it.
	NoPKCE bool `json:"noPkce,omitempty"`
	// JSONToken sends token requests as JSON instead of a form.
	JSONToken bool `json:"jsonToken,omitempty"`
	// Extra are added to the authorize URL (such as access_type=offline).
	Extra map[string]string `json:"extra,omitempty"`
}

const (
	KindAPIKey      = "apikey"
	KindOAuthCode   = "oauth-code"
	KindOAuthDevice = "oauth-device"
)

var defID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// Normalize fills defaults and checks a def; the error names the field.
func (d *Def) Normalize() error {
	d.ID = strings.TrimSpace(strings.ToLower(d.ID))
	d.BaseURL = strings.TrimRight(strings.TrimSpace(d.BaseURL), "/")
	if !defID.MatchString(d.ID) {
		return errors.New("id: lowercase letters, digits and -, up to 40")
	}
	if _, ok := registry[d.ID]; ok {
		return fmt.Errorf("id: %q is a built-in provider", d.ID)
	}
	if d.Name == "" {
		d.Name = d.ID
	}
	if d.Kind == "" {
		d.Kind = KindAPIKey
	}
	if d.API == "" {
		d.API = "openai"
	}
	switch d.API {
	case "openai", "anthropic", "responses":
	default:
		return errors.New("api: openai, anthropic or responses")
	}
	if !httpURL(strings.ReplaceAll(d.BaseURL, "{accountId}", "x")) {
		return errors.New("baseUrl: an http(s) URL")
	}
	if d.ModelsURL != "" && d.ModelsURL != "none" && !httpURL(strings.ReplaceAll(d.ModelsURL, "{accountId}", "x")) {
		return errors.New("modelsUrl: an http(s) URL, or none")
	}
	if d.Icon != "" && !strings.HasPrefix(d.Icon, "https://") && !strings.HasPrefix(d.Icon, "data:image/") {
		return errors.New("icon: an https: or data:image/ URL")
	}
	if d.Color != "" && !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(d.Color) {
		return errors.New("color: #rrggbb")
	}
	for k := range d.Headers {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k, " :\r\n") {
			return fmt.Errorf("headers: %q is not a header name", k)
		}
	}
	models := d.Models[:0]
	for _, m := range d.Models {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}
	d.Models = models
	if d.ModelsURL == "none" && len(d.Models) == 0 {
		return errors.New("models: list the models when the provider has no model list")
	}
	switch d.Kind {
	case KindAPIKey:
		d.OAuth = nil
	case KindOAuthCode, KindOAuthDevice:
		o := d.OAuth
		if o == nil {
			return errors.New("oauth: the sign-in settings are required")
		}
		if o.ClientID == "" {
			return errors.New("oauth.clientId is required")
		}
		if !httpURL(o.TokenURL) {
			return errors.New("oauth.tokenUrl: an http(s) URL")
		}
		// verifyUrl becomes the href of the device sign-in link, so a
		// javascript: or data: value would be executable in the dashboard.
		if o.VerifyURL != "" && !httpURL(o.VerifyURL) {
			return errors.New("oauth.verifyUrl: an http(s) URL, or none")
		}
		if d.Kind == KindOAuthCode {
			if !httpURL(o.AuthorizeURL) {
				return errors.New("oauth.authorizeUrl: an http(s) URL")
			}
			if o.RedirectURI == "" {
				return errors.New("oauth.redirectUri is required (the one the provider's app registered)")
			}
		} else if !httpURL(o.DeviceCodeURL) {
			return errors.New("oauth.deviceCodeUrl: an http(s) URL")
		}
	default:
		return errors.New("kind: apikey, oauth-code or oauth-device")
	}
	return nil
}

// Provider is how intact calls a declared provider.
func (d Def) Provider() Provider {
	p := Provider{ID: d.ID, BaseURL: d.BaseURL, AuthHeader: d.AuthHeader, AuthPrefix: d.AuthPrefix,
		Models: d.Models, Defaults: map[string]string{}}
	if d.API != "openai" {
		p.API = d.API
	}
	oauth := d.Kind != KindAPIKey
	if p.AuthHeader == "" {
		if d.API == "anthropic" && !oauth {
			p.AuthHeader, p.AuthPrefix = "X-Api-Key", ""
		} else {
			p.AuthHeader, p.AuthPrefix = "Authorization", "Bearer "
		}
	}
	if d.API == "anthropic" {
		p.Defaults["Anthropic-Version"] = "2023-06-01"
	}
	for k, v := range d.Headers {
		p.Defaults[k] = v
	}
	switch {
	case oauth:
		p.Setup = "oauth"
	case strings.Contains(d.BaseURL, "{accountId}"):
		p.Setup = "account"
	}
	switch d.ModelsURL {
	case "":
	case "none":
		p.NoModelList = true
	default:
		p.ModelsURL = d.ModelsURL
	}
	return p
}

// declared holds the defs in use, set by the server from the database.
var declared struct {
	sync.RWMutex
	defs map[string]Def
}

// SetDeclared replaces the declared providers.
func SetDeclared(defs []Def) {
	m := map[string]Def{}
	for _, d := range defs {
		m[d.ID] = d
	}
	declared.Lock()
	declared.defs = m
	declared.Unlock()
}

// Declared returns a declared provider's def.
func Declared(id string) (Def, bool) {
	declared.RLock()
	defer declared.RUnlock()
	d, ok := declared.defs[id]
	return d, ok
}

// DeclaredIDs lists the declared providers.
func DeclaredIDs() []string {
	declared.RLock()
	defer declared.RUnlock()
	out := make([]string, 0, len(declared.defs))
	for id := range declared.defs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
