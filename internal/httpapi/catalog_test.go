package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/louisphamdev/intact/internal/store"
)

// modelListStub serves /models and counts the requests. gate blocks a request
// until the test releases it.
func modelListStub(t *testing.T, hits *atomic.Int32, gate func(n int32, w http.ResponseWriter) bool) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if gate != nil && gate(n, w) {
			return
		}
		w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func catalogAPI(t *testing.T, url string) *api {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.CreateConnection("groq", "a", "ka")
	a, _ := newServer(s, map[string]string{"groq": url}, nil)
	return a
}

// A caller that goes away must not take the fetch with it: the answer still
// reaches the cache, so the next caller gets the list.
func TestCatalogFetchSurvivesACancelledCaller(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	url := modelListStub(t, &hits, func(n int32, w http.ResponseWriter) bool {
		if n == 1 {
			close(started)
			<-release
		}
		return false
	})
	a := catalogAPI(t, url)

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan []string, 1)
	go func() { got <- a.catalogIDs(ctx, "groq") }()
	<-started
	cancel()
	select {
	case ids := <-got:
		if len(ids) != 0 {
			t.Errorf("the cancelled caller got %v, want nothing", ids)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled caller did not return")
	}

	close(release)
	if ids := a.catalogIDs(context.Background(), "groq"); len(ids) != 2 {
		t.Errorf("the next call got %v, want the full list", ids)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream list requests = %d, want 1", n)
	}
}

// A failed refresh must not empty a list that still works, and a good fetch
// that lands while the failing one is still out must win.
func TestCatalogKeepsTheGoodListWhenARefreshFails(t *testing.T) {
	defer catalogFetchTimeout.Set(150 * time.Millisecond)()

	var hits atomic.Int32
	slow := make(chan struct{})
	url := modelListStub(t, &hits, func(n int32, w http.ResponseWriter) bool {
		if n == 2 {
			<-slow
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})
	a := catalogAPI(t, url)

	if ids := a.catalogIDs(context.Background(), "groq"); len(ids) != 2 {
		t.Fatalf("first fetch got %v, want the full list", ids)
	}
	a.cat.mu.Lock()
	e := a.cat.m["groq"]
	e.at = time.Now().Add(-time.Hour)
	a.cat.m["groq"] = e
	a.cat.mu.Unlock()

	// The second fetch is slow and fails; it gives up on its own timeout.
	if ids := a.catalogIDs(context.Background(), "groq"); len(ids) != 2 {
		t.Errorf("a failed refresh served %v, want the previous list", ids)
	}
	// A good fetch runs while the failing one is still out, and wins.
	if ids := a.refreshCatalog(context.Background(), "groq"); len(ids) != 2 {
		t.Errorf("the refresh got %v, want the full list", ids)
	}
	close(slow)
	time.Sleep(100 * time.Millisecond)

	a.cat.mu.Lock()
	defer a.cat.mu.Unlock()
	if ids := a.cat.m["groq"].ids; len(ids) != 2 {
		t.Errorf("cached ids = %v, want the good list", ids)
	}
}

// Concurrent misses share one upstream call.
func TestCatalogSharesOneFetchAcrossConcurrentMisses(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	url := modelListStub(t, &hits, func(n int32, w http.ResponseWriter) bool {
		<-release
		return false
	})
	a := catalogAPI(t, url)

	var wg sync.WaitGroup
	out := make([][]string, 12)
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = a.catalogIDs(context.Background(), "groq")
		}(i)
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, ids := range out {
		if len(ids) != 2 {
			t.Errorf("caller %d got %v, want the full list", i, ids)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream list requests = %d, want exactly 1", n)
	}
}
