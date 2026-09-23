package httpapi

import "testing"

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
