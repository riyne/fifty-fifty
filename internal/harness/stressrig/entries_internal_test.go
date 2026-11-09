package stressrig

import (
	"testing"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/ledger"
)

// A posting with the right entries plus a balanced pair for two other members still sums
// to zero, and every wanted entry is present, so only the entry count tells it apart.
func TestSameEntries(t *testing.T) {
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	want := []ledger.Entry{{MemberID: a, Amount: -30}, {MemberID: b, Amount: 30}}
	cases := []struct {
		name string
		got  map[uuid.UUID]int64
		ok   bool
	}{
		{"exact", map[uuid.UUID]int64{a: -30, b: 30}, true},
		{"amount differs", map[uuid.UUID]int64{a: -29, b: 29}, false},
		{"member missing", map[uuid.UUID]int64{a: -30}, false},
		{"extra balanced pair", map[uuid.UUID]int64{a: -30, b: 30, c: 5, d: -5}, false},
		{"empty", map[uuid.UUID]int64{}, false},
	}
	for _, tc := range cases {
		if got := sameEntries(tc.got, want); got != tc.ok {
			t.Errorf("%s: sameEntries = %v, want %v", tc.name, got, tc.ok)
		}
	}
	if !sameEntries(nil, nil) {
		t.Error("a posting with no entries (the payer ate everything) must match an empty expectation")
	}
}
