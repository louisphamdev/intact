package httpapi

import (
	"testing"
	"time"
)

func TestKeyLimiterSlidesOverSixtySeconds(t *testing.T) {
	l := keyLimiter{seen: map[string][]time.Time{}}
	t0 := time.Unix(1_000_000, 0)
	for i, at := range []time.Duration{0, 20 * time.Second} {
		if ok, _ := l.allow("k", 2, t0.Add(at)); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	// Third within the minute: refused until the first one is 60 s old.
	if ok, wait := l.allow("k", 2, t0.Add(30*time.Second)); ok || wait != 30*time.Second {
		t.Errorf("third: ok=%v wait=%v, want refused for 30s", ok, wait)
	}
	if n := l.count("k", t0.Add(30*time.Second)); n != 2 {
		t.Errorf("count = %d, the refused request must not count", n)
	}
	if ok, _ := l.allow("k", 2, t0.Add(61*time.Second)); !ok {
		t.Error("refused after the first request left the window")
	}
	// A key that went quiet holds no memory.
	l.count("k", t0.Add(10*time.Minute))
	if _, held := l.seen["k"]; held {
		t.Error("an idle key is still held")
	}
}
