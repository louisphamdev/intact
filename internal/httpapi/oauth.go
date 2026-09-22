package httpapi

import (
	"context"
	"log"
	"time"

	"github.com/louisphamdev/intact/internal/oauth"
)

// secretFor returns the credential to use for a connection. For an OAuth
// connection whose access token is at or near expiry, it refreshes the token,
// stores the new one, and returns it. A refresh failure falls back to the token
// on file, which may still work for a short while.
func (a *api) secretFor(ctx context.Context, connID string) (string, error) {
	creds, err := a.store.OAuth(connID)
	if err == nil && creds.TokenURL != "" && needsRefresh(creds.ExpiresAt) {
		tok, newRT, exp, rerr := oauth.Refresh(ctx, creds.TokenURL, creds.ClientID, creds.ClientSecret, creds.RefreshToken)
		if rerr != nil {
			log.Printf("refresh oauth for connection %s: %v", connID, rerr)
			go a.notify(EventAccountAuth, "auth|"+connID, 6*time.Hour, notifyMsg{Title: "An account's sign-in failed to renew",
				Lines: []string{"Account: " + connID, "Error: " + truncate(rerr.Error(), 200), "Sign in again on the provider's page."},
				Path:  "#/providers"})
		} else if uerr := a.store.UpdateAfterRefresh(connID, tok, newRT, exp.UTC().Format(time.RFC3339)); uerr != nil {
			log.Printf("store refreshed token for connection %s: %v", connID, uerr)
		} else {
			return tok, nil
		}
	}
	return a.store.Secret(connID)
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
	tok, newRT, exp, rerr := oauth.Refresh(ctx, c.TokenURL, c.ClientID, c.ClientSecret, c.RefreshToken)
	if rerr != nil {
		log.Printf("force refresh oauth for connection %s: %v", connID, rerr)
		return "", false
	}
	if uerr := a.store.UpdateAfterRefresh(connID, tok, newRT, exp.UTC().Format(time.RFC3339)); uerr != nil {
		log.Printf("store force-refreshed token for connection %s: %v", connID, uerr)
		return "", false
	}
	return tok, true
}
