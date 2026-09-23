package store

import (
	"path/filepath"
	"testing"
)

func TestSyncModelsSoftDeletesUnlisted(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.SyncModels("groq", []string{"a", "b"}, true, nil)
	s.SetModelsActive("groq", []string{"a"}, false)
	s.RecordModelTest("groq", "a", true, 42, "ok", "conn1")

	// The provider lists only "b" now: "a" must survive as stale, not be deleted.
	s.SyncModels("groq", []string{"b"}, true, nil)
	a := findModel(t, s, "a")
	if !a.Stale {
		t.Error("unlisted model was not marked stale")
	}
	if a.Active {
		t.Error("unlisted model lost the operator's off toggle")
	}
	if a.TestMs != 42 || a.TestConn != "conn1" {
		t.Errorf("unlisted model lost its test history: %+v", a)
	}

	// When "a" comes back, it is no longer stale.
	s.SyncModels("groq", []string{"a", "b"}, true, nil)
	if findModel(t, s, "a").Stale {
		t.Error("reappeared model is still stale")
	}
}

func findModel(t *testing.T, s *Store, model string) ProviderModel {
	t.Helper()
	list, err := s.ListModels("groq")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("model %q not found (was it deleted?)", model)
	return ProviderModel{}
}
