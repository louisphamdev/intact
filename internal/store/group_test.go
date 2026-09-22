package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestGroupSaveListReplaceDelete(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateConnection("groq", "a", "k1")
	b, _ := s.CreateConnection("nvidia", "b", "k2")

	g := Group{Name: "fast", Strategy: StrategyRoundRobin, Members: []Member{
		{ConnectionID: b.ID, Model: "meta/llama"}, {ConnectionID: a.ID}, {ConnectionID: a.ID},
	}}
	if err := s.SaveGroup(g); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGroup("fast")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 2 || got.Members[0].ConnectionID != b.ID || got.Members[0].Model != "meta/llama" {
		t.Fatalf("members = %+v, want b then a, duplicate dropped", got.Members)
	}

	// Replace: new strategy and order.
	g = Group{Name: "fast", Strategy: StrategyFallback, Members: []Member{{ConnectionID: a.ID}}}
	if err := s.SaveGroup(g); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetGroup("fast")
	if got.Strategy != StrategyFallback || len(got.Members) != 1 {
		t.Fatalf("after replace = %+v", got)
	}

	// Deleting a connection removes it from its groups.
	if err := s.DeleteConnection(a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetGroup("fast")
	if len(got.Members) != 0 {
		t.Fatalf("members after connection delete = %+v, want none", got.Members)
	}

	if err := s.DeleteGroup("fast"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGroup("fast"); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("get after delete err = %v", err)
	}
}

func TestGroupRejectsBadInput(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "t.db"))
	defer s.Close()
	if err := s.SaveGroup(Group{Name: "Bad Name", Strategy: StrategyRoundRobin}); err == nil {
		t.Error("accepted a name with a space")
	}
	if err := s.SaveGroup(Group{Name: "ok", Strategy: "random"}); err == nil {
		t.Error("accepted an unknown strategy")
	}
	err := s.SaveGroup(Group{Name: "ok", Strategy: StrategyRoundRobin, Members: []Member{{ConnectionID: "nope"}}})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown member err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetGroup("ok"); err == nil {
		t.Error("a failed save left a group behind")
	}
}

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
