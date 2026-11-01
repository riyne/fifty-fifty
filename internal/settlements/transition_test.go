package settlements_test

import (
	"testing"

	"github.com/riyne/fifty-fifty/internal/settlements"
)

// Every (status, outcome) pair, written out. If the state machine changes, this
// table is what has to change with it.
func TestTransition(t *testing.T) {
	const (
		apply    = settlements.Apply
		ignore   = settlements.Ignore
		conflict = settlements.Conflict
	)
	tests := []struct {
		from settlements.Status
		kind settlements.Kind
		want settlements.Action
	}{
		{settlements.Pending, settlements.KindSucceeded, apply},
		{settlements.Pending, settlements.KindFailed, apply},
		{settlements.Pending, settlements.KindUnknown, apply},

		{settlements.Unknown, settlements.KindSucceeded, apply},
		{settlements.Unknown, settlements.KindFailed, apply},
		{settlements.Unknown, settlements.KindUnknown, ignore},

		{settlements.Succeeded, settlements.KindSucceeded, ignore}, // duplicate
		{settlements.Succeeded, settlements.KindFailed, conflict},
		{settlements.Succeeded, settlements.KindUnknown, ignore},

		{settlements.Failed, settlements.KindFailed, ignore}, // duplicate
		{settlements.Failed, settlements.KindSucceeded, conflict},
		{settlements.Failed, settlements.KindUnknown, ignore},
	}
	if len(tests) != 4*3 {
		t.Fatalf("table has %d rows, want all 12 (status, outcome) pairs", len(tests))
	}
	for _, tc := range tests {
		if got := settlements.Transition(tc.from, tc.kind); got != tc.want {
			t.Errorf("%s + %s = %s, want %s", tc.from, tc.kind, got, tc.want)
		}
	}
}

// Whatever sequence of outcomes arrives, a terminal status never changes, and the
// only way to reach succeeded is through an Apply of a success.
func TestTransition_TerminalIsForever(t *testing.T) {
	kinds := []settlements.Kind{settlements.KindSucceeded, settlements.KindFailed, settlements.KindUnknown}
	for _, terminal := range []settlements.Status{settlements.Succeeded, settlements.Failed} {
		for _, k := range kinds {
			if settlements.Transition(terminal, k) == settlements.Apply {
				t.Errorf("%s + %s would be applied", terminal, k)
			}
		}
	}
}
