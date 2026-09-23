package httpapi

import (
	"testing"
	"time"
)

func TestLoginGuardPerIPBlocksAfterMax(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	for i := 0; i < loginPerIPMax; i++ {
		if ok, _ := g.allow("1.1.1.1", now); !ok {
			t.Fatalf("attempt %d blocked too early", i)
		}
		g.fail("1.1.1.1", now)
	}
	ok, wait := g.allow("1.1.1.1", now)
	if ok || wait <= 0 {
		t.Fatalf("source not blocked after %d failures: ok=%v wait=%v", loginPerIPMax, ok, wait)
	}
	// A different source is unaffected.
	if ok, _ := g.allow("2.2.2.2", now); !ok {
		t.Fatalf("second source blocked by the first's failures")
	}
}

func TestLoginGuardSucceedResets(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	for i := 0; i < loginPerIPMax; i++ {
		g.fail("1.1.1.1", now)
	}
	g.succeed("1.1.1.1")
	if ok, _ := g.allow("1.1.1.1", now); !ok {
		t.Fatalf("source still blocked after a correct code")
	}
}

func TestLoginGuardGlobalCap(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	// Rotate the source each time so the per-IP limit never triggers; only the
	// global cap can stop this.
	for i := 0; i < loginGlobalMax; i++ {
		g.fail(itoaIP(i), now)
	}
	if ok, _ := g.allow("fresh-source", now); ok {
		t.Fatalf("global cap did not stop a source-rotating brute force")
	}
	// The window clears with time.
	if ok, _ := g.allow("fresh-source", now.Add(loginGlobalWin+time.Second)); !ok {
		t.Fatalf("global cap did not clear after its window")
	}
}

func itoaIP(i int) string {
	return "10.0.0." + string(rune('0'+i%10)) + "-" + string(rune('a'+i/10))
}
