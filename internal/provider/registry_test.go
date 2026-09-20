package provider

import "testing"

func TestLookupReturnsClassBProviders(t *testing.T) {
	p, ok := Lookup("groq")
	if !ok {
		t.Fatal("groq is not registered")
	}
	if p.BaseURL != "https://api.groq.com/openai/v1" {
		t.Errorf("BaseURL = %q", p.BaseURL)
	}
	if p.AuthHeader != "Authorization" || p.AuthPrefix != "Bearer " {
		t.Errorf("auth = %q %q, want Authorization / Bearer ", p.AuthHeader, p.AuthPrefix)
	}

	if _, ok := Lookup("antigravity"); ok {
		t.Error("antigravity must not be registered in phase 1: it is class A")
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("an unknown id must not resolve")
	}
}
