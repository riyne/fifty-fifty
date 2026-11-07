package receiptapi

import (
	"testing"
	"time"
)

func TestSecondsUntilUTCMidnight(t *testing.T) {
	for _, tc := range []struct {
		now  time.Time
		want int
	}{
		{time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC), 86400},
		{time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), 43200},
		{time.Date(2026, 11, 6, 23, 59, 59, 0, time.UTC), 1},
		{time.Date(2026, 11, 6, 23, 59, 59, 500_000_000, time.UTC), 1}, // rounded up, never 0
		{time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC), 3600},         // over a year end
		// The same instant in another zone is the same answer: the quota day is UTC.
		{time.Date(2026, 11, 6, 15, 0, 0, 0, time.FixedZone("PST", -8*3600)), 3600},
	} {
		if got := secondsUntilUTCMidnight(tc.now); got != tc.want {
			t.Errorf("%v: %d, want %d", tc.now, got, tc.want)
		}
	}
}
