package httpapi

import (
	"os"
	"sync"
	"time"
)

// intact keeps stored timestamps in UTC, which is correct for comparison. The
// one value a person reads as a calendar day is the usage total, so its day
// boundary must follow the operator's clock, not UTC. On a GMT+7 server a UTC
// day rolls the total over at 07:00 local; a local day rolls it at midnight.

var (
	tzOnce sync.Once
	tzLoc  *time.Location
)

// reportLocation is the zone the usage day boundary follows. INTACT_TZ names an
// IANA zone (for example "Asia/Bangkok"); when it is empty or unknown, the zone
// is a fixed GMT+7, which is Vietnam's offset all year (no daylight saving).
func reportLocation() *time.Location {
	tzOnce.Do(func() {
		if name := os.Getenv("INTACT_TZ"); name != "" {
			if loc, err := time.LoadLocation(name); err == nil {
				tzLoc = loc
				return
			}
		}
		tzLoc = time.FixedZone("+07", 7*60*60)
	})
	return tzLoc
}

// usageDay is the calendar day of t in the report zone, as "2006-01-02".
func usageDay(t time.Time) string { return usageDayIn(t, reportLocation()) }

// usageDayIn is the testable core of usageDay.
func usageDayIn(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02")
}
