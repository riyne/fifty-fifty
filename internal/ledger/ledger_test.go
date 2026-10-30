package ledger_test

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/allocator"
	"github.com/riyne/fifty-fifty/internal/ledger"
)

func id(b byte) uuid.UUID { return uuid.UUID{b} }

func sum(es []ledger.Entry) (n int64) {
	for _, e := range es {
		n += e.Amount
	}
	return n
}

func TestExpenseEntries_KnownCases(t *testing.T) {
	payer, a, b := id(1), id(2), id(3)
	tests := []struct {
		name   string
		shares []allocator.Member
		want   []ledger.Entry
	}{
		{
			name:   "payer did not claim anything",
			shares: []allocator.Member{{ID: a, Total: 700}, {ID: b, Total: 300}},
			want:   []ledger.Entry{{payer, 1000}, {a, -700}, {b, -300}},
		},
		{
			name:   "payer also claimed: credited only what the others owe",
			shares: []allocator.Member{{ID: payer, Total: 500}, {ID: a, Total: 700}, {ID: b, Total: 300}},
			want:   []ledger.Entry{{payer, 1000}, {a, -700}, {b, -300}},
		},
		{
			name:   "payer ate everything: no entries",
			shares: []allocator.Member{{ID: payer, Total: 2000}},
			want:   nil,
		},
		{
			name:   "a member with a zero share has no entry",
			shares: []allocator.Member{{ID: a, Total: 0}, {ID: b, Total: 300}},
			want:   []ledger.Entry{{payer, 300}, {b, -300}},
		},
		{name: "nothing at all", shares: nil, want: nil},
	}
	for _, tc := range tests {
		got, err := ledger.ExpenseEntries(payer, tc.shares)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got  %v\n want %v", tc.name, got, tc.want)
		}
	}
}

func TestExpenseEntries_Errors(t *testing.T) {
	if _, err := ledger.ExpenseEntries(id(1), []allocator.Member{{ID: id(2), Total: -1}}); !errors.Is(err, ledger.ErrNegativeShare) {
		t.Errorf("negative share: %v", err)
	}
	huge := []allocator.Member{{ID: id(2), Total: 1 << 62}, {ID: id(3), Total: 1 << 62}, {ID: id(4), Total: 1 << 62}}
	if _, err := ledger.ExpenseEntries(id(1), huge); !errors.Is(err, ledger.ErrOverflow) {
		t.Errorf("overflow: %v", err)
	}
}

// Whatever the shares, a posting sums to exactly zero, never contains a zero
// entry, has at most one entry per member, is sorted, and debits each non-payer
// exactly their share.
func TestExpenseEntries_Properties(t *testing.T) {
	r := rand.New(rand.NewPCG(20261029, 2))
	for iter := range 20000 {
		n := r.IntN(8)
		shares := make([]allocator.Member, n)
		var ids []uuid.UUID
		for i := range shares {
			var m uuid.UUID
			for k := range m {
				m[k] = byte(r.IntN(256))
			}
			ids = append(ids, m)
			total := int64(0)
			if r.IntN(4) != 0 {
				total = r.Int64N(1_000_000)
			}
			shares[i] = allocator.Member{ID: m, Total: total}
		}
		payer := uuid.UUID{0xff, byte(iter)}
		if n > 0 && r.IntN(2) == 0 {
			payer = ids[r.IntN(n)] // the payer claimed something too
		}

		es, err := ledger.ExpenseEntries(payer, shares)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		if s := sum(es); s != 0 {
			t.Fatalf("iter %d: entries sum to %d: %v", iter, s, es)
		}
		seen := map[uuid.UUID]bool{}
		for _, e := range es {
			if e.Amount == 0 || seen[e.MemberID] {
				t.Fatalf("iter %d: zero or duplicate entry: %v", iter, es)
			}
			seen[e.MemberID] = true
		}
		for _, s := range shares {
			if s.ID == payer || s.Total == 0 {
				continue
			}
			found := false
			for _, e := range es {
				if e.MemberID == s.ID {
					found = e.Amount == -s.Total
				}
			}
			if !found {
				t.Fatalf("iter %d: member %s with share %d is not debited exactly that: %v", iter, s.ID, s.Total, es)
			}
		}
		if !slices.IsSortedFunc(es, func(a, b ledger.Entry) int { return slices.Compare(a.MemberID[:], b.MemberID[:]) }) {
			t.Fatalf("iter %d: not sorted by member id", iter)
		}
	}
}
