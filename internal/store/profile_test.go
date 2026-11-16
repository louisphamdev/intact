package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func newProfileStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "profile_test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProfileCRUD(t *testing.T) {
	s := newProfileStore(t)

	p, err := s.CreateProfile("groq", "lite", []string{"a", "b"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Provider != "groq" || p.Name != "lite" || len(p.Members) != 2 {
		t.Fatalf("got %+v", p)
	}

	list, err := s.ListProfiles()
	if err != nil || len(list) != 1 || list[0].ID != p.ID {
		t.Fatalf("list: %v %+v", err, list)
	}

	// The same name on another provider is a different profile.
	if _, err := s.CreateProfile("openai", "lite", nil); err != nil {
		t.Fatalf("create on another provider: %v", err)
	}
	// The same name on the same provider is refused.
	if _, err := s.CreateProfile("groq", "lite", nil); !errors.Is(err, ErrProfileExists) {
		t.Fatalf("duplicate: %v, want ErrProfileExists", err)
	}

	p, err = s.UpdateProfile(p.ID, "small", []string{"c"})
	if err != nil || p.Name != "small" || len(p.Members) != 1 || p.Members[0] != "c" {
		t.Fatalf("update: %v %+v", err, p)
	}
	// An empty name and nil members keep their values.
	p, err = s.UpdateProfile(p.ID, "", nil)
	if err != nil || p.Name != "small" || len(p.Members) != 1 {
		t.Fatalf("update keep: %v %+v", err, p)
	}

	got, err := s.Profile(p.ID)
	if err != nil || got.Name != "small" {
		t.Fatalf("profile: %v %+v", err, got)
	}

	if err := s.DeleteProfile(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteProfile(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v, want ErrNotFound", err)
	}
	if _, err := s.Profile(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("profile after delete: %v, want ErrNotFound", err)
	}
}

func TestUpdateProfileRefusesADuplicateName(t *testing.T) {
	s := newProfileStore(t)
	first, _ := s.CreateProfile("groq", "lite", nil)
	if _, err := s.CreateProfile("groq", "tiny", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.UpdateProfile(first.ID, "tiny", nil); !errors.Is(err, ErrProfileExists) {
		t.Fatalf("rename to an existing name: %v, want ErrProfileExists", err)
	}
}

func TestProfileNotFound(t *testing.T) {
	s := newProfileStore(t)
	if _, err := s.Profile("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("profile: %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateProfile("nope", "x", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update: %v, want ErrNotFound", err)
	}
}
