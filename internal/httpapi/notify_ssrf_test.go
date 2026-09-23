package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

func TestBlockedHostRejectsInternalTargets(t *testing.T) {
	blocked := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/x",
		"http://localhost/x",
		"http://10.0.0.5/x",
		"http://192.168.1.1/x",
		"http://172.16.0.1/x",
		"http://[::1]/x",
		"http://0.0.0.0/x",
		"http://box.local/x",
		"not a url",
	}
	for _, u := range blocked {
		if !blockedHost(u) {
			t.Errorf("blockedHost(%q) = false, want it blocked", u)
		}
	}
	allowed := []string{
		"https://hooks.slack.com/services/x",
		"https://example.com/webhook",
		"http://public.example.org:8443/x",
	}
	for _, u := range allowed {
		if blockedHost(u) {
			t.Errorf("blockedHost(%q) = true, want it allowed", u)
		}
	}
}

// allowDial lets a test reach an httptest server, which always listens on
// loopback. Only what the test asserts may then stop the request.
func allowDial(t *testing.T) {
	t.Helper()
	old := dialGuard
	dialGuard = func(string, string, syscall.RawConn) error { return nil }
	t.Cleanup(func() { dialGuard = old })
}

// loopbackName rewrites an httptest URL to a name that resolves to loopback.
// blockedHost lets the name through, so the dial-time check is what is tested.
func loopbackName(t *testing.T, srvURL string) string {
	t.Helper()
	target := strings.Replace(srvURL, "127.0.0.1", "localhost.", 1)
	if blockedHost(target) {
		t.Fatalf("blockedHost(%q) already refuses the name; the dial check stays untested", target)
	}
	return target
}

func TestWebhookDialGuardRejectsInternalAddresses(t *testing.T) {
	refused := []string{
		"127.0.0.1:80", "10.66.66.1:443", "169.254.169.254:80",
		"[::1]:80", "192.168.1.1:80", "172.16.0.1:80", "0.0.0.0:80",
		"[fe80::1]:80", "[fc00::1]:80", "224.0.0.1:80", "no-port",
	}
	for _, addr := range refused {
		if err := dialGuard("tcp", addr, nil); err == nil {
			t.Errorf("dialGuard(%q) = nil, want it refused", addr)
		}
	}
	allowed := []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443"}
	for _, addr := range allowed {
		if err := dialGuard("tcp", addr, nil); err != nil {
			t.Errorf("dialGuard(%q) = %v, want it allowed", addr, err)
		}
	}
}

func TestWebhookRefusesANameThatResolvesToLoopback(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	err := sendWebhook(context.Background(), map[string]string{"url": loopbackName(t, srv.URL)}, notifyMsg{Title: "t"})
	if !errors.Is(err, errPrivateTarget) {
		t.Fatalf("sendWebhook = %v, want the private-address error", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("the server got %d requests, want 0", n)
	}
}

func TestWebhookDoesNotFollowARedirect(t *testing.T) {
	allowDial(t)
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		var second int32
		dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&second, 1)
		}))
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, dst.URL, code)
		}))
		err := sendWebhook(context.Background(), map[string]string{"url": loopbackName(t, first.URL)}, notifyMsg{Title: "t"})
		if err == nil {
			t.Errorf("%d: sendWebhook = nil, want the redirect reported as a failure", code)
		}
		if n := atomic.LoadInt32(&second); n != 0 {
			t.Errorf("%d: the redirect target got %d requests, want 0", code, n)
		}
		first.Close()
		dst.Close()
	}
}

// freePort opens a listener to take a port, then closes it. Nothing answers on
// that port, so a connection to it is refused.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	return port
}

// stalledPort accepts a connection and never answers, so the client waits for
// its own timeout.
func stalledPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// The channel test button must not tell an operator which internal ports are
// open. The three ways a send can fail before an answer arrives must read the
// same, over HTTP and over MCP.
func TestNotifyChannelTestIsNotAPortScanOracle(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	cfg := authConfig()
	h := NewWithAuth(s, nil, cfg)
	tok := cfg.APIToken

	guarded, closed, stalled := freePort(t), freePort(t), stalledPort(t)
	old := dialGuard
	dialGuard = func(network, address string, c syscall.RawConn) error {
		if _, p, _ := net.SplitHostPort(address); p == guarded {
			return errPrivateTarget
		}
		return nil
	}
	oldClient := webhookClient
	webhookClient = &http.Client{Timeout: 400 * time.Millisecond, Transport: guardedTransport(),
		CheckRedirect: oldClient.CheckRedirect}
	t.Cleanup(func() { dialGuard = old; webhookClient = oldClient })

	create := func(port string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"name": "w" + port, "type": "webhook", "enabled": true,
			"config": map[string]string{"url": "http://localhost.:" + port + "/hook"}})
		req := loopbackRequest("POST", "/api/notify/channels", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var c store.NotifyChannel
		json.Unmarshal(rec.Body.Bytes(), &c)
		if c.ID == "" {
			t.Fatalf("create channel on port %s: %d %s", port, rec.Code, rec.Body.String())
		}
		return c.ID
	}
	overHTTP := func(id string) string {
		t.Helper()
		req := loopbackRequest("POST", "/api/notify/channels/"+id+"/test", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Fatalf("test over HTTP: code=%d body=%s, want a failure", rec.Code, rec.Body.String())
		}
		var m map[string]string
		json.Unmarshal(rec.Body.Bytes(), &m)
		return m["error"]
	}
	overMCP := func(id string) string {
		t.Helper()
		m := rpc(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"test_notify_channel","arguments":{"id":"`+id+`"}}}`)
		text, isErr := toolText(t, m)
		if !isErr {
			t.Fatalf("test over MCP: %s, want a tool error", text)
		}
		return text
	}

	var texts []string
	for _, port := range []string{guarded, closed, stalled} {
		id := create(port)
		texts = append(texts, overHTTP(id), overMCP(id))
	}
	for _, got := range texts {
		if got != texts[0] {
			t.Errorf("answers differ: %q vs %q", got, texts[0])
		}
		if strings.ContainsAny(got, "0123456789") || strings.Contains(got, "127.0.0.1") {
			t.Errorf("the answer %q carries a status code or a dial detail", got)
		}
	}
}
