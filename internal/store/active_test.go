package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSetActive(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	c, _ := s.CreateConnection("groq", "a", "k")
	if err := s.SetActive(c.ID, false); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListConnections()
	if list[0].IsActive {
		t.Error("still active")
	}
	if err := s.SetActive("nope", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id err = %v", err)
	}
}
