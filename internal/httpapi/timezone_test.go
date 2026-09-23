package httpapi

import (
	"os"
	"sync"
	"testing"
	"time"
)

func TestUsageDayIn(t *testing.T) {
	// 2026-01-01 23:30 UTC is 2026-01-02 06:30 in GMT+7: the request counts
	// toward the second, not the first.
	in := time.Date(2026, 1, 1, 23, 30, 0, 0, time.UTC)
	if got := usageDayIn(in, time.FixedZone("+07", 7*60*60)); got != "2026-01-02" {
		t.Fatalf("GMT+7 day = %s, want 2026-01-02", got)
	}
	if got := usageDayIn(in, time.UTC); got != "2026-01-01" {
		t.Fatalf("UTC day = %s, want 2026-01-01", got)
	}
}

func TestReportLocationDefaultsToGMT7(t *testing.T) {
	os.Unsetenv("INTACT_TZ")
	tzOnce = sync.Once{}
	loc := reportLocation()
	_, offset := time.Now().In(loc).Zone()
	if offset != 7*60*60 {
		t.Fatalf("default offset = %d, want %d", offset, 7*60*60)
	}
}
