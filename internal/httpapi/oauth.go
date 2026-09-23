package httpapi

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/oauth"
	"github.com/louisphamdev/intact/internal/store"
)

// refreshLocks serializes token refreshes per connection. Without it, several
// requests that meet the same expired token each refresh, and a provider that
// rotates its refresh token invalidates every copy but one, which loses the
// account. One refresh at a time, and the others read the stored result.
type refreshLocks struct {
	mu        sync.Mutex
	m         map[string]*sync.Mutex
	overrides map[string]tokenOverride
}

// tokenOverride holds a refreshed token whose store write failed. It is served
// until a later write succeeds, so the rotated refresh token is not lost.
type tokenOverride struct {
	accessToken  string
	refreshToken string
	expiresAt    string
}

func (r *refreshLocks) lock(id string) *sync.Mutex {
	r.mu.Lock()
	if r.m == nil {
		r.m = map[string]*sync.Mutex{}
	}
	l := r.m[id]
	if l == nil {
		l = &sync.Mutex{}
		r.m[id] = l
	}
	r.mu.Unlock()
	l.Lock()
	return l
}

func (r *refreshLocks) tryLock(id string) (*sync.Mutex, bool) {
	r.mu.Lock()
	if r.m == nil {
		r.m = map[string]*sync.Mutex{}
	}
	l := r.m[id]
	if l == nil {
		l = &sync.Mutex{}
		r.m[id] = l
	}
	r.mu.Unlock()
	if !l.TryLock() {
		return nil, false
	}
	return l, true
}

func (r *refreshLocks) getOverride(id string) (tokenOverride, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overrides == nil {
		return tokenOverride{}, false
	}
	ov, ok := r.overrides[id]
	return ov, ok
}

func (r *refreshLocks) setOverride(id string, ov tokenOverride) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overrides == nil {
		r.overrides = map[string]tokenOverride{}
	}
	r.overrides[id] = ov
}

func (r *refreshLocks) clearOverride(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overrides != nil {
		delete(r.overrides, id)
	}
}

func (a *api) applyOverride(creds *store.OAuthCreds, connID string) {
	if ov, ok := a.refresh.getOverride(connID); ok && ov.accessToken != "" {
		creds.ExpiresAt = ov.expiresAt
		if ov.refreshToken != "" {
			creds.RefreshToken = ov.refreshToken
		}
	}
}

func (a *api) currentSecret(connID string) (string, error) {
	if ov, ok := a.refresh.getOverride(connID); ok && ov.accessToken != "" {
		return ov.accessToken, nil
	}
	return a.store.Secret(connID)
}

// secretFor returns the credential to use for a connection. For an OAuth
// connection whose access token is at or near expiry, it refreshes the token,
// stores the new one, and returns it. A failed exchange falls back to the token
// on file; a failed write retains the new token in memory under refreshLocks.
func (a *api) secretFor(ctx context.Context, connID string) (string, error) {
	creds, err := a.store.OAuth(connID)
	if err != nil || creds.TokenURL == "" {
		return a.currentSecret(connID)
	}
	a.applyOverride(&creds, connID)
	if !needsRefresh(creds.ExpiresAt) {
		return a.currentSecret(connID)
	}
	l := a.refresh.lock(connID)
	defer l.Unlock()
	// Re-read under the lock: another request may have refreshed while this one
	// waited, so it must not refresh again and reuse a rotated token.
	if creds, err = a.store.OAuth(connID); err != nil {
		return a.currentSecret(connID)
	}
	a.applyOverride(&creds, connID)
	if !needsRefresh(creds.ExpiresAt) {
		return a.currentSecret(connID)
	}
	rctx, cancel := refreshContext(ctx)
	defer cancel()
	tok, newRT, exp, rerr := oauth.Refresh(rctx, creds.TokenURL, creds.ClientID, creds.ClientSecret,
		creds.RefreshToken, jsonTokenBody(a.providerOf(connID)))
	if rerr != nil {
		log.Printf("refresh oauth for connection %s: %v", connID, rerr)
		go a.notify(EventAccountAuth, "auth|"+connID, 6*time.Hour, notifyMsg{Title: "An account's sign-in failed to renew",
			Lines: []string{"Account: " + connID, "Error: " + truncate(rerr.Error(), 200), "Sign in again on the provider's page."},
			Path:  "#/providers"})
		return a.currentSecret(connID)
	}
	effRT := newRT
	if effRT == "" {
		effRT = creds.RefreshToken
	}
	expStr := exp.UTC().Format(time.RFC3339)
	if uerr := a.store.UpdateAfterRefresh(connID, tok, newRT, expStr); uerr != nil {
		a.refresh.setOverride(connID, tokenOverride{
			accessToken:  tok,
			refreshToken: effRT,
			expiresAt:    expStr,
		})
		a.alertUnsavedToken(connID, uerr)
	} else {
		a.refresh.clearOverride(connID)
	}
	return tok, nil
}

// refreshContext detaches a token exchange from the caller. A client that
// disconnects mid-exchange must not cancel it: the provider may have rotated the
// refresh token already, and dropping the answer loses the account.
func refreshContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// providerOf returns the provider id of a connection, or "" when it is unknown.
func (a *api) providerOf(connID string) string {
	list, err := a.store.ListConnections()
	if err != nil {
		return ""
	}
	for _, c := range list {
		if c.ID == connID {
			return c.Provider
		}
	}
	return ""
}

// alertUnsavedToken reports a refresh that the store did not keep. This process
// holds the only copy of the new token, so a restart loses the account.
func (a *api) alertUnsavedToken(connID string, err error) {
	log.Printf("store refreshed token for connection %s: %v", connID, err)
	go a.notify(EventAccountAuth, "authsave|"+connID, 6*time.Hour, notifyMsg{
		Title: "An account renewed its sign-in, but the new token was not saved",
		Lines: []string{"Account: " + connID, "Error: " + truncate(err.Error(), 200),
			"intact uses the new token now. A restart loses it. Sign in again on the provider's page."},
		Path: "#/providers"})
}

// needsRefresh reports whether an access token should be refreshed. An unknown
// expiry is left alone: refreshing on every call would hammer the token
// endpoint. A known expiry within 60 s counts as due.
func needsRefresh(expiresAt string) bool {
	if expiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return false
	}
	return time.Now().Add(60 * time.Second).After(t)
}

// isOAuth reports whether a connection is configured for OAuth refresh.
func (a *api) isOAuth(connID string) bool {
	c, err := a.store.OAuth(connID)
	return err == nil && c.TokenURL != ""
}

// forceRefresh refreshes an OAuth token regardless of its recorded expiry. It is
// the recovery path for a token the provider revoked before it was due to
// expire. It returns the new token and whether a refresh happened.
func (a *api) forceRefresh(ctx context.Context, connID string) (string, bool) {
	c, err := a.store.OAuth(connID)
	if err != nil || c.TokenURL == "" {
		return "", false
	}
	prev, _ := a.currentSecret(connID)
	l := a.refresh.lock(connID)
	defer l.Unlock()
	// Another request that met the same 401 may have refreshed while this one
	// waited: use its token rather than refresh again and rotate twice.
	if cur, _ := a.currentSecret(connID); cur != "" && cur != prev {
		return cur, true
	}
	if c, err = a.store.OAuth(connID); err != nil || c.TokenURL == "" {
		return "", false
	}
	a.applyOverride(&c, connID)
	rctx, cancel := refreshContext(ctx)
	defer cancel()
	tok, newRT, exp, rerr := oauth.Refresh(rctx, c.TokenURL, c.ClientID, c.ClientSecret,
		c.RefreshToken, jsonTokenBody(a.providerOf(connID)))
	if rerr != nil {
		log.Printf("force refresh oauth for connection %s: %v", connID, rerr)
		return "", false
	}
	effRT := newRT
	if effRT == "" {
		effRT = c.RefreshToken
	}
	expStr := exp.UTC().Format(time.RFC3339)
	if uerr := a.store.UpdateAfterRefresh(connID, tok, newRT, expStr); uerr != nil {
		a.refresh.setOverride(connID, tokenOverride{
			accessToken:  tok,
			refreshToken: effRT,
			expiresAt:    expStr,
		})
		a.alertUnsavedToken(connID, uerr)
	} else {
		a.refresh.clearOverride(connID)
	}
	return tok, true
}
