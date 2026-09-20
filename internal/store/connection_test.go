package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectionRoundTripAndSecretIsolation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c, err := s.CreateConnection("groq", "work key", "gsk-secret-value")
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	if c.ID == "" {
		t.Fatal("CreateConnection returned an empty id")
	}

	list, err := s.ListConnections()
	if err != nil {
		t.Fatalf("ListConnections: %v", err)
	}
	if len(list) != 1 || list[0].Provider != "groq" {
		t.Fatalf("ListConnections = %+v, want one groq row", list)
	}

	// The secret must not be reachable through the listing type, even by
	// serializing it. This is the guard the design calls for.
	blob, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "gsk-secret-value") {
		t.Errorf("secret leaked through the listing: %s", blob)
	}

	got, err := s.Secret(c.ID)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if got != "gsk-secret-value" {
		t.Errorf("Secret = %q, want the stored value", got)
	}

	if _, err := s.Secret("no-such-id"); err == nil {
		t.Error("Secret on an unknown id returned no error")
	}
}
