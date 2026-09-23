package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPendingLoginTakenExactlyOnce(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutPendingLogin(PendingLogin{State: "st", Provider: "p", Verifier: "v"}); err != nil {
		t.Fatal(err)
	}
	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.TakePendingLogin("st"); ok {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("pending login consumed %d times, want exactly 1", wins)
	}
}

func TestSetMetaConcurrentMergeKeepsEveryKey(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.CreateConnection("p", "l", "sec")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("k%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.SetMeta(c.ID, map[string]string{k: "v"})
		}()
	}
	wg.Wait()
	list, err := s.ListConnections()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.ID == c.ID {
			if len(x.Meta) != 20 {
				t.Errorf("meta has %d keys, want 20: a concurrent merge dropped keys", len(x.Meta))
			}
		}
	}
}
