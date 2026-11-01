package ledger_test

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/riyne/fifty-fifty/internal/ledger"
)

func TestSettlementEntries(t *testing.T) {
	a, b := id(1), id(2)

	// The sender owes (negative balance), so they are credited; the payee is debited.
	got, err := ledger.SettlementEntries(a, b, 700)
	if err != nil {
		t.Fatal(err)
	}
	if want := []ledger.Entry{{a, 700}, {b, -700}}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Sorted by member id whichever way round the pair is.
	got, err = ledger.SettlementEntries(b, a, 700)
	if err != nil {
		t.Fatal(err)
	}
	if want := []ledger.Entry{{a, -700}, {b, 700}}; !slices.Equal(got, want) {
		t.Errorf("reversed: got %v, want %v", got, want)
	}

	// The largest amount still sums to exactly zero.
	got, err = ledger.SettlementEntries(a, b, math.MaxInt64)
	if err != nil || sum(got) != 0 {
		t.Errorf("max amount: entries %v, sum %d, err %v", got, sum(got), err)
	}
}

func TestSettlementEntries_Rejects(t *testing.T) {
	a, b := id(1), id(2)
	for name, tc := range map[string]struct {
		from, to [16]byte
		amount   int64
	}{
		"same member":     {a, a, 100},
		"zero amount":     {a, b, 0},
		"negative amount": {a, b, -5},
		"min int64":       {a, b, math.MinInt64},
	} {
		if _, err := ledger.SettlementEntries(tc.from, tc.to, tc.amount); !errors.Is(err, ledger.ErrBadSettlement) {
			t.Errorf("%s: err = %v, want ErrBadSettlement", name, err)
		}
	}
}
