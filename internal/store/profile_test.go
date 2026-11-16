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

	p, err := s.CreateProfile("groq", "lite", []string{"a", "b"}, []string{"llama"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Provider != "groq" || p.Name != "lite" || len(p.Members) != 2 {
		t.Fatalf("got %+v", p)
	}
	if len(p.Models) != 1 || p.Models[0] != "llama" {
		t.Fatalf("models: %+v", p.Models)
	}

	list, err := s.ListProfiles()
	if err != nil || len(list) != 1 || list[0].ID != p.ID {
		t.Fatalf("list: %v %+v", err, list)
	}

	// The same name on another provider is a different profile.
	if _, err := s.CreateProfile("openai", "lite", nil, nil); err != nil {
		t.Fatalf("create on another provider: %v", err)
	}
	// The same name on the same provider is refused.
	if _, err := s.CreateProfile("groq", "lite", nil, nil); !errors.Is(err, ErrProfileExists) {
		t.Fatalf("duplicate: %v, want ErrProfileExists", err)
	}

	p, err = s.UpdateProfile(p.ID, ProfileUpdate{Name: strptr("small"),
		Members: &[]string{"c"}, Models: &[]string{"llama", "mixtral"}})
	if err != nil || p.Name != "small" || len(p.Members) != 1 || p.Members[0] != "c" {
		t.Fatalf("update: %v %+v", err, p)
	}
	if len(p.Models) != 2 || p.Models[1] != "mixtral" {
		t.Fatalf("update models: %+v", p.Models)
	}
	// An empty name and a nil members keep their values; an empty models list
	// clears the profile's own list.
	p, err = s.UpdateProfile(p.ID, ProfileUpdate{Name: strptr(""), Members: nil,
		Models: &[]string{}})
	if err != nil || p.Name != "small" || len(p.Members) != 1 {
		t.Fatalf("update keep: %v %+v", err, p)
	}
	if p.Models != nil {
		t.Fatalf("cleared models: %+v, want nil", p.Models)
	}

	got, err := s.Profile(p.ID)
	if err != nil || got.Name != "small" || got.Models != nil {
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

// A profile created before the models column existed keeps its members and
// serves the provider's whole list: the column reads back empty.
func TestProfileModelsColumnMigrates(t *testing.T) {
	s := newProfileStore(t)
	if _, err := s.DB.Exec(
		`INSERT INTO profiles (id, provider, name, members, created_at, updated_at)
		 VALUES ('old1', 'groq', 'old', '["a"]', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	p, err := s.Profile("old1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(p.Members) != 1 || p.Members[0] != "a" || p.Models != nil {
		t.Fatalf("migrated profile: %+v", p)
	}
	if _, err := s.CreateProfile("groq", "new", nil, []string{"llama"}); err != nil {
		t.Fatalf("create after migration: %v", err)
	}
}

func TestUpdateProfileRefusesADuplicateName(t *testing.T) {
	s := newProfileStore(t)
	first, _ := s.CreateProfile("groq", "lite", nil, nil)
	if _, err := s.CreateProfile("groq", "tiny", nil, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.UpdateProfile(first.ID, ProfileUpdate{Name: strptr("tiny")}); !errors.Is(err, ErrProfileExists) {
		t.Fatalf("rename to an existing name: %v, want ErrProfileExists", err)
	}
}

func TestProfileNotFound(t *testing.T) {
	s := newProfileStore(t)
	if _, err := s.Profile("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("profile: %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateProfile("nope", ProfileUpdate{Name: strptr("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update: %v, want ErrNotFound", err)
	}
}

func strptr(s string) *string { return &s }
