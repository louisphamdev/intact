package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/louisphamdev/intact/internal/store"
)

// A stored secret belongs to the destination and the channel type it was
// entered for. These cases pin both guards, so a later edit of saveChannel
// cannot send the operator's bot token or bearer to an address of a caller's
// choosing.
func TestNotifySecretIsNotCarriedToANewDestinationOrType(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	h := New(s, nil)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest(method, path, strings.NewReader(body)))
		return rec
	}
	stored := func(id string) store.NotifyChannel {
		t.Helper()
		list, _ := s.ListNotifyChannels()
		for _, c := range list {
			if c.ID == id {
				return c
			}
		}
		t.Fatalf("channel %s is gone", id)
		return store.NotifyChannel{}
	}

	var tg store.NotifyChannel
	json.Unmarshal(do("POST", "/notify/channels", `{"name":"tg","type":"telegram","config":{"botToken":"123:ABCDEFGHIJ","chatId":"-100"}}`).Body.Bytes(), &tg)
	if tg.ID == "" || tg.Config["botToken"] != "••••GHIJ" {
		t.Fatalf("create = %+v", tg)
	}

	// The chat id moves, so the masked bot token must not follow it.
	rec := do("PUT", "/notify/channels/"+tg.ID, `{"type":"telegram","config":{"botToken":"••••GHIJ","chatId":"-200"}}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Bot token is required") {
		t.Errorf("moved chat id: %d %s, want 400 and \"Bot token is required\"", rec.Code, rec.Body.String())
	}
	if got := stored(tg.ID); got.Config["chatId"] != "-100" || got.Config["botToken"] != "123:ABCDEFGHIJ" {
		t.Errorf("a refused write changed the stored channel: %+v", got.Config)
	}

	// The same destination keeps the stored token, so the mask stays usable.
	if rec := do("PUT", "/notify/channels/"+tg.ID, `{"name":"tg2","type":"telegram","config":{"botToken":"••••GHIJ","chatId":"-100"}}`); rec.Code != 200 {
		t.Fatalf("same destination: %d %s", rec.Code, rec.Body.String())
	}
	if got := stored(tg.ID).Config["botToken"]; got != "123:ABCDEFGHIJ" {
		t.Errorf("stored botToken = %q, want the carried token", got)
	}

	// The type changes, so the mask must never reach the store as a credential.
	if rec := do("PUT", "/notify/channels/"+tg.ID, `{"type":"webhook","config":{"url":"https://hooks.example.com/services/T/B/X","secret":"••••GHIJ"}}`); rec.Code != 200 {
		t.Fatalf("type change: %d %s", rec.Code, rec.Body.String())
	}
	if got := stored(tg.ID).Config["secret"]; got != "" {
		t.Errorf("stored secret = %q, want none", got)
	}

	// A row written before the channel type changed still holds the fields of
	// both types, so the destination is unchanged and only the type guard is
	// left to stop the carry-over.
	old, err := s.SaveNotifyChannel(store.NotifyChannel{Name: "legacy", Type: "telegram", Enabled: true,
		Config: map[string]string{"botToken": "123:ABCDEFGHIJ", "chatId": "-100",
			"url": "https://hooks.example.com/services/T/B/Z", "secret": "bearer-1234567890"}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rec := do("PUT", "/notify/channels/"+old.ID, `{"type":"webhook","config":{"url":"https://hooks.example.com/services/T/B/Z","secret":"••••7890"}}`); rec.Code != 200 {
		t.Fatalf("legacy type change: %d %s", rec.Code, rec.Body.String())
	}
	if got := stored(old.ID).Config["secret"]; got != "" {
		t.Errorf("stored secret = %q, want none: the type changed", got)
	}
}
