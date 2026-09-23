// Package oauth refreshes an expired OAuth access token with the refresh_token
// grant. Providers differ in detail, so the caller passes each field and the
// body encoding. A provider that needs different fields is handled by its own
// strategy, not by widening this function.
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second}

// Refresh exchanges a refresh token for a new access token. It returns the new
// token and its absolute expiry. client_secret is sent only when it is not empty
// (Google needs it; the public PKCE clients do not). jsonBody must match the
// encoding the provider's sign-in exchange uses, or the provider answers 400.
func Refresh(ctx context.Context, tokenURL, clientID, clientSecret, refreshToken string, jsonBody bool) (accessToken, newRefreshToken string, expiresAt time.Time, err error) {
	fields := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     clientID,
	}
	if clientSecret != "" {
		fields["client_secret"] = clientSecret
	}
	var req *http.Request
	if jsonBody {
		b, _ := json.Marshal(fields)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", time.Time{}, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", time.Time{}, fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", "", time.Time{}, fmt.Errorf("token endpoint returned no access_token")
	}
	ttl := out.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	return out.AccessToken, out.RefreshToken, time.Now().Add(time.Duration(ttl) * time.Second), nil
}
