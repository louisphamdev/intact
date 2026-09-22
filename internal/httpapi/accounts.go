package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/louisphamdev/intact/internal/provider"
)

// accounts lists the connections so a caller can choose an id.
// The listing type carries no credential, which is what keeps this endpoint safe.
func (a *api) accounts(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListConnections()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read connections")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"accounts": list})
}

// customID is the shape of a custom provider id, which prefixes model names.
var customID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// createAccount stores a new connection from the dashboard form, then returns
// to the dashboard. The secret is taken by value and never echoed back.
//
// A registered provider may need an account id (Cloudflare), no key at all, or
// an OAuth sign-in. Any other provider id is a custom OpenAI-compatible
// provider and must come with its base URL.
func (a *api) createAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad form")
		return
	}
	prov := strings.TrimSpace(r.PostFormValue("provider"))
	// A pasted key often carries a newline or a stray space, which no
	// provider accepts in a header.
	secret := strings.TrimSpace(r.PostFormValue("secret"))
	baseURL := strings.TrimRight(strings.TrimSpace(r.PostFormValue("base_url")), "/")
	p, registered := provider.Lookup(prov)
	switch {
	case prov == "":
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	case registered && p.Setup == "account":
		acct := strings.TrimSpace(r.PostFormValue("account_id"))
		if acct == "" || secret == "" {
			writeError(w, http.StatusBadRequest, "account id and key are required")
			return
		}
		baseURL = p.WithAccount(acct)
	case registered && p.Setup == "oauth":
		writeError(w, http.StatusBadRequest, "this provider's accounts sign in with OAuth; they cannot be added with a key")
		return
	case registered && p.Setup == "none":
		secret = ""
	case registered:
		if secret == "" {
			writeError(w, http.StatusBadRequest, "key is required")
			return
		}
	default:
		if !customID.MatchString(prov) {
			writeError(w, http.StatusBadRequest, "provider id must be lowercase letters, digits or -")
			return
		}
		if u, err := url.Parse(baseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			writeError(w, http.StatusBadRequest, "a custom provider needs an http(s) base URL")
			return
		}
	}
	c, err := a.store.CreateConnection(prov, r.PostFormValue("label"), secret)
	if err == nil && baseURL != "" {
		err = a.store.SetBaseURL(c.ID, baseURL)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create connection")
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// deleteAccount removes one connection, then returns to the dashboard.
func (a *api) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteConnection(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// usage serves the daily token counters, newest day first.
func (a *api) usage(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.Usage()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read usage")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"usage": rows})
}

// modelsForAccount fetches the provider's model list for one connection, from
// the server side so the dashboard (which holds a session, not the bearer token)
// can show it. It returns the provider's own JSON unchanged.
func (a *api) modelsForAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	conn, err := a.connection(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	if _, ok := a.providerFor(conn); !ok {
		writeError(w, http.StatusNotFound, "provider not supported in this build")
		return
	}
	resp, err := a.getModels(r.Context(), conn)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
