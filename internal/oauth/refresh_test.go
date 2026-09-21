package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefreshSendsGrantAndParsesToken(t *testing.T) {
	var gotGrant, gotRefresh, gotClient, gotSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotGrant = r.PostFormValue("grant_type")
		gotRefresh = r.PostFormValue("refresh_token")
		gotClient = r.PostFormValue("client_id")
		gotSecret = r.PostFormValue("client_secret")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"new-token","refresh_token":"rotated-rt","expires_in":3600}`))
	}))
	defer srv.Close()

	tok, newRT, exp, err := Refresh(context.Background(), srv.URL, "cid", "csecret", "rt-1")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok != "new-token" {
		t.Errorf("access token = %q, want new-token", tok)
	}
	if newRT != "rotated-rt" {
		t.Errorf("rotated refresh token = %q, want rotated-rt", newRT)
	}
	if d := time.Until(exp); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("expiry in %v, want ~1h from now", d)
	}
	if gotGrant != "refresh_token" || gotRefresh != "rt-1" || gotClient != "cid" || gotSecret != "csecret" {
		t.Errorf("form sent grant=%q refresh=%q client=%q secret=%q", gotGrant, gotRefresh, gotClient, gotSecret)
	}
}

func TestRefreshOmitsEmptySecret(t *testing.T) {
	var hadSecret bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		_, hadSecret = r.PostForm["client_secret"]
		w.Write([]byte(`{"access_token":"t","expires_in":60}`))
	}))
	defer srv.Close()
	if _, _, _, err := Refresh(context.Background(), srv.URL, "cid", "", "rt"); err != nil {
		t.Fatal(err)
	}
	if hadSecret {
		t.Error("client_secret was sent even though it is empty")
	}
}

func TestRefreshErrorsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	if _, _, _, err := Refresh(context.Background(), srv.URL, "cid", "", "rt"); err == nil {
		t.Error("Refresh returned no error on 401")
	}
}
