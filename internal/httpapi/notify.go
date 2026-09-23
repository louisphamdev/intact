package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// Alerts go to the channels the owner declares: what intact did on its own
// (a rule added, a model or an account switched off), what it could not fix,
// and a review that stopped. A channel is a type and the fields of that
// type, so a new kind of channel is a new entry in notifyTypes.

// Events a channel can take. A channel with no events takes them all.
const (
	EventErrorAction     = "error.action"     // the error review acted
	EventErrorUnresolved = "error.unresolved" // the error review could not fix a group
	EventErrorBurst      = "error.burst"      // a group of errors grew fast
	EventDriftAction     = "drift.action"     // the drift review blacklisted a field
	EventReviewPaused    = "review.paused"    // a review's model failed; it waits
	EventAccountAuth     = "account.auth"     // an account's sign-in no longer works
	EventTest            = "test"
)

var notifyEvents = []map[string]string{
	{"id": EventErrorAction, "label": "The error review acted: a rule added, a model or an account switched off"},
	{"id": EventErrorUnresolved, "label": "The error review could not fix a group of errors"},
	{"id": EventErrorBurst, "label": "A group of errors grew fast"},
	{"id": EventDriftAction, "label": "The drift review blacklisted a field"},
	{"id": EventReviewPaused, "label": "A review stopped: its model cannot be reached"},
	{"id": EventAccountAuth, "label": "An account's sign-in stopped working"},
}

// notifyField is one field of a channel type.
type notifyField struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Help     string `json:"help,omitempty"`
	Required bool   `json:"required,omitempty"`
	Secret   bool   `json:"secret,omitempty"`
	Pattern  string `json:"pattern,omitempty"` // "int" or "url"
}

type notifyType struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Fields []notifyField `json:"fields"`
	send   func(ctx context.Context, cfg map[string]string, m notifyMsg) error
}

var linkField = notifyField{ID: "link", Label: "Dashboard address", Pattern: "url",
	Help: "Alerts link back here, such as https://intact.example. Empty: no link."}

var notifyTypes = []notifyType{
	{ID: "telegram", Name: "Telegram", Fields: []notifyField{
		{ID: "botToken", Label: "Bot token", Required: true, Secret: true, Help: "From @BotFather. The bot must be a member of the chat."},
		{ID: "chatId", Label: "Chat id", Required: true, Help: "A user, group or channel id, such as -1001234567890."},
		{ID: "threadId", Label: "Topic id", Pattern: "int", Help: "For a forum group: the topic's message_thread_id. Empty: the general topic."},
		linkField,
	}, send: sendTelegram},
	{ID: "webhook", Name: "Webhook", Fields: []notifyField{
		{ID: "url", Label: "URL", Required: true, Pattern: "url", Help: `intact POSTs {"event","title","lines","at","link"} as JSON.`},
		{ID: "secret", Label: "Bearer token", Secret: true, Help: "Sent as Authorization: Bearer. Optional."},
		linkField,
	}, send: sendWebhook},
}

func notifyTypeOf(id string) (notifyType, bool) {
	for _, t := range notifyTypes {
		if t.ID == id {
			return t, true
		}
	}
	return notifyType{}, false
}

// notifyMsg is one alert.
type notifyMsg struct {
	Event string   `json:"event"`
	Title string   `json:"title"`
	Lines []string `json:"lines"`
	At    string   `json:"at"`
	Path  string   `json:"-"` // dashboard page, such as "#/errors"
	Link  string   `json:"link,omitempty"`
}

var telegramAPI = "https://api.telegram.org"

func sendTelegram(ctx context.Context, cfg map[string]string, m notifyMsg) error {
	var b strings.Builder
	b.WriteString("🛡 <b>" + html.EscapeString(m.Title) + "</b>")
	for _, l := range m.Lines {
		b.WriteString("\n" + html.EscapeString(l))
	}
	if m.Link != "" {
		b.WriteString("\n<a href=\"" + html.EscapeString(m.Link) + "\">intact</a>")
	}
	text := b.String()
	if len(text) > 4000 {
		text = text[:4000] + "…"
	}
	form := url.Values{"chat_id": {cfg["chatId"]}, "text": {text}, "parse_mode": {"HTML"}, "disable_web_page_preview": {"true"}}
	if cfg["threadId"] != "" {
		form.Set("message_thread_id", cfg["threadId"])
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, telegramAPI+"/bot"+cfg["botToken"]+"/sendMessage", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("telegram unreachable") // the error text would carry the token
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		var d struct {
			Description string `json:"description"`
		}
		json.Unmarshal(raw, &d)
		return fmt.Errorf("telegram %d: %s", resp.StatusCode, d.Description)
	}
	return nil
}

// blockedHost reports a webhook target that must not be reached: a loopback,
// private, or link-local address (including the cloud metadata endpoint
// 169.254.169.254), so a webhook cannot be turned into a request against an
// internal service.
func blockedHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return true
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	return false
}

func sendWebhook(ctx context.Context, cfg map[string]string, m notifyMsg) error {
	if blockedHost(cfg["url"]) {
		return errors.New("webhook target is a private or loopback address")
	}
	body, _ := json.Marshal(m)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg["url"], bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg["secret"] != "" {
		req.Header.Set("Authorization", "Bearer "+cfg["secret"])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook answered %d", resp.StatusCode)
	}
	return nil
}

// notifier holds when each alert key was last sent, so a repeating problem
// alerts once per window.
type notifier struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// notify sends an alert to every enabled channel that takes the event. A
// non-empty key sends at most once per window.
func (a *api) notify(event, key string, window time.Duration, m notifyMsg) {
	if key != "" {
		a.notes.mu.Lock()
		if a.notes.last == nil {
			a.notes.last = map[string]time.Time{}
		}
		if t, ok := a.notes.last[key]; ok && time.Since(t) < window {
			a.notes.mu.Unlock()
			return
		}
		a.notes.last[key] = time.Now()
		a.notes.mu.Unlock()
	}
	m.Event = event
	m.At = time.Now().UTC().Format(time.RFC3339)
	list, _ := a.store.ListNotifyChannels()
	var wg sync.WaitGroup
	for _, c := range list {
		if !c.Enabled || (len(c.Events) > 0 && event != EventTest && !contains(c.Events, event)) {
			continue
		}
		wg.Add(1)
		go func(c store.NotifyChannel) {
			defer wg.Done()
			a.sendTo(c, m)
		}(c)
	}
	wg.Wait()
}

func (a *api) sendTo(c store.NotifyChannel, m notifyMsg) error {
	t, ok := notifyTypeOf(c.Type)
	if !ok {
		return errors.New("unknown channel type " + c.Type)
	}
	if l := strings.TrimRight(c.Config["link"], "/"); l != "" {
		m.Link = l + "/" + m.Path
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := t.send(ctx, c.Config, m)
	msg := ""
	if err != nil {
		msg = err.Error()
		log.Printf("notify %s (%s): %v", c.Name, c.Type, err)
	}
	a.store.NoteNotifySent(c.ID, msg)
	return err
}

// ---- API

func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return "••••"
	}
	return "••••" + v[len(v)-4:]
}

func masked(c store.NotifyChannel) store.NotifyChannel {
	t, _ := notifyTypeOf(c.Type)
	cfg := map[string]string{}
	for k, v := range c.Config {
		cfg[k] = v
	}
	for _, f := range t.Fields {
		if f.Secret {
			cfg[f.ID] = maskSecret(cfg[f.ID])
		}
	}
	c.Config = cfg
	return c
}

// notifyInfo serves the channels (secrets masked), the channel types and
// the events.
func (a *api) notifyInfo(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListNotifyChannels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read channels")
		return
	}
	for i := range list {
		list[i] = masked(list[i])
	}
	writeJSON(w, map[string]any{"channels": list, "types": notifyTypes, "events": notifyEvents})
}

// saveChannel checks a channel against its type and stores it. A secret
// left empty or masked keeps the stored one.
func (a *api) saveChannel(c store.NotifyChannel) (store.NotifyChannel, error) {
	t, ok := notifyTypeOf(c.Type)
	if !ok {
		return c, errors.New("type: one of telegram, webhook")
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		c.Name = t.Name
	}
	var old store.NotifyChannel
	if c.ID != "" {
		list, _ := a.store.ListNotifyChannels()
		found := false
		for _, x := range list {
			if x.ID == c.ID {
				old, found = x, true
			}
		}
		if !found {
			return c, errors.New("no such channel")
		}
		c.Created = old.Created
	}
	// A secret is bound to the destination it is sent to. When any destination
	// field changes (a webhook url, a telegram chat id), do not carry the stored
	// secret to the new target: a caller could otherwise redirect the operator's
	// bot token or bearer to their own address. A changed destination forces the
	// secret to be entered again.
	destChanged := false
	for _, f := range t.Fields {
		if f.Secret || f.ID == linkField.ID {
			continue
		}
		if strings.TrimSpace(c.Config[f.ID]) != old.Config[f.ID] {
			destChanged = true
		}
	}
	cfg := map[string]string{}
	for _, f := range t.Fields {
		v := strings.TrimSpace(c.Config[f.ID])
		if f.Secret && (v == "" || strings.HasPrefix(v, "••••")) && old.Type == c.Type && !destChanged {
			v = old.Config[f.ID]
		}
		switch {
		case v == "" && f.Required:
			return c, fmt.Errorf("%s is required", f.Label)
		case v != "" && f.Pattern == "int":
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				return c, fmt.Errorf("%s: a number", f.Label)
			}
		case v != "" && f.Pattern == "url":
			if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return c, fmt.Errorf("%s: an http(s) address", f.Label)
			}
			// A webhook posts to this address, so it must not point inside the host.
			if f.ID != linkField.ID && blockedHost(v) {
				return c, fmt.Errorf("%s: a public address, not a private or loopback one", f.Label)
			}
		}
		if v != "" {
			cfg[f.ID] = v
		}
	}
	c.Config = cfg
	var ev []string
	for _, e := range c.Events {
		for _, k := range notifyEvents {
			if k["id"] == e && !contains(ev, e) {
				ev = append(ev, e)
			}
		}
	}
	c.Events = ev
	return a.store.SaveNotifyChannel(c)
}

func (a *api) putChannel(w http.ResponseWriter, r *http.Request) {
	var c store.NotifyChannel
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "bad json")
		return
	}
	if id := r.PathValue("id"); id != "" {
		c.ID = id
	}
	saved, err := a.saveChannel(c)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, masked(saved))
}

func (a *api) deleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteNotifyChannel(r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot delete")
		return
	}
	writeJSON(w, map[string]any{"deleted": r.PathValue("id")})
}

// testChannel sends a test alert to one channel and says how it went.
func (a *api) testChannel(w http.ResponseWriter, r *http.Request) {
	list, _ := a.store.ListNotifyChannels()
	for _, c := range list {
		if c.ID == r.PathValue("id") {
			err := a.sendTo(c, notifyMsg{Event: EventTest, Title: "Test alert", At: time.Now().UTC().Format(time.RFC3339),
				Lines: []string{"This channel will receive intact's alerts."}, Path: "#/alerts"})
			if err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			writeJSON(w, map[string]any{"sent": true})
			return
		}
	}
	writeError(w, http.StatusNotFound, "no such channel")
}
