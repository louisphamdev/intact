package store

import (
	"path/filepath"
	"testing"
)

func TestSyncModelsDeletesUnlisted(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.SyncModels("groq", []string{"a", "b"}, true, nil)

	// A fallback list is not the provider's answer: it deletes nothing.
	s.SyncModels("groq", []string{"b"}, false, nil)
	if !hasModel(t, s, "a") {
		t.Error("a fallback list deleted a model")
	}

	// The provider lists only "b" now: "a" is gone.
	s.SyncModels("groq", []string{"b"}, true, nil)
	if hasModel(t, s, "a") {
		t.Error("a model the provider no longer lists was kept")
	}
	if !hasModel(t, s, "b") {
		t.Error("a listed model was deleted")
	}
}

func hasModel(t *testing.T, s *Store, model string) bool {
	t.Helper()
	list, err := s.ListModels("groq")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Model == model {
			return true
		}
	}
	return false
}
