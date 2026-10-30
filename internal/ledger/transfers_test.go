package ledger_test

import (
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/ledger"
)

func bal(b byte, amount int64) ledger.Balance { return ledger.Balance{MemberID: id(b), Amount: amount} }

// apply returns each member's balance after the transfers are made: the payer's
// balance goes up (they owe less), the receiver's goes down (they are owed less).
func apply(bs []ledger.Balance, ts []ledger.Transfer) map[uuid.UUID]int64 {
	out := map[uuid.UUID]int64{}
	for _, b := range bs {
		out[b.MemberID] = b.Amount
	}
	for _, t := range ts {
		out[t.From] += t.Amount
		out[t.To] -= t.Amount
	}
	return out
}

func TestSuggestTransfers_KnownCases(t *testing.T) {
	tests := []struct {
		name string
		in   []ledger.Balance
		want []ledger.Transfer
	}{
		{"nobody owes anything", []ledger.Balance{bal(1, 0), bal(2, 0)}, nil},
		{"empty", nil, nil},
		{"one debt", []ledger.Balance{bal(1, 500), bal(2, -500)}, []ledger.Transfer{{id(2), id(1), 500}}},
		{
			// A chain: c owes b 1000, b owes a 1000 collapses to one payment.
			"a chain collapses", []ledger.Balance{bal(1, 1000), bal(2, 0), bal(3, -1000)},
			[]ledger.Transfer{{id(3), id(1), 1000}},
		},
		{
			// Biggest creditor is paid by biggest debtor first (1 by 3, 600); then the
			// biggest remaining creditor (2, 300) by the biggest remaining debtor (4, 400).
			"largest pairs first", []ledger.Balance{bal(1, 700), bal(2, 300), bal(3, -600), bal(4, -400)},
			[]ledger.Transfer{{id(3), id(1), 600}, {id(4), id(2), 300}, {id(4), id(1), 100}},
		},
		{
			// Equal amounts: the lowest id is matched first.
			"ties go to the lowest id", []ledger.Balance{bal(4, 100), bal(1, 100), bal(3, -100), bal(2, -100)},
			[]ledger.Transfer{{id(2), id(1), 100}, {id(3), id(4), 100}},
		},
	}
	for _, tc := range tests {
		got, err := ledger.SuggestTransfers(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got  %v\n want %v", tc.name, got, tc.want)
		}
	}
}

func TestSuggestTransfers_Errors(t *testing.T) {
	if _, err := ledger.SuggestTransfers([]ledger.Balance{bal(1, 100), bal(2, -99)}); !errors.Is(err, ledger.ErrNotBalanced) {
		t.Errorf("unbalanced: %v", err)
	}
	if _, err := ledger.SuggestTransfers([]ledger.Balance{bal(1, 5), bal(1, -5)}); !errors.Is(err, ledger.ErrDuplicate) {
		t.Errorf("duplicate id: %v", err)
	}
	if _, err := ledger.SuggestTransfers([]ledger.Balance{bal(1, math.MaxInt64), bal(2, 1), bal(3, -5)}); !errors.Is(err, ledger.ErrOverflow) {
		t.Errorf("overflow: %v", err)
	}
	if _, err := ledger.SuggestTransfers([]ledger.Balance{bal(1, math.MinInt64), bal(2, math.MaxInt64)}); !errors.Is(err, ledger.ErrOverflow) {
		t.Errorf("MinInt64: %v", err)
	}
}

// randBalances makes n nonzero-or-zero balances that sum to exactly zero.
func randBalances(r *rand.Rand, n int) []ledger.Balance {
	bs := make([]ledger.Balance, n)
	var sum int64
	for i := range n - 1 {
		bs[i].MemberID = uuid.UUID{byte(i + 1)}
		if r.IntN(5) != 0 {
			bs[i].Amount = r.Int64N(2001) - 1000
		}
		sum += bs[i].Amount
	}
	bs[n-1] = ledger.Balance{MemberID: uuid.UUID{byte(n)}, Amount: -sum}
	return bs
}

func TestSuggestTransfers_Properties(t *testing.T) {
	r := rand.New(rand.NewPCG(20261029, 3))
	for iter := range 20000 {
		n := 1 + r.IntN(12)
		bs := randBalances(r, n)
		ts, err := ledger.SuggestTransfers(bs)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}

		// 1. Doing the transfers settles everyone.
		for m, left := range apply(bs, ts) {
			if left != 0 {
				t.Fatalf("iter %d: member %s still at %d after the transfers\n balances %v\n transfers %v", iter, m, left, bs, ts)
			}
		}
		// 2. Every transfer is a positive payment from someone who owes to someone owed.
		start := map[uuid.UUID]int64{}
		var credits, moved int64
		nonzero := 0
		for _, b := range bs {
			start[b.MemberID] = b.Amount
			if b.Amount > 0 {
				credits += b.Amount
			}
			if b.Amount != 0 {
				nonzero++
			}
		}
		for _, tr := range ts {
			moved += tr.Amount
			if tr.Amount <= 0 || start[tr.From] >= 0 || start[tr.To] <= 0 || tr.From == tr.To {
				t.Fatalf("iter %d: bad transfer %+v for balances %v", iter, tr, bs)
			}
		}
		// 3. No more than n-1 transfers, and exactly the credited amount moves.
		if limit := max(nonzero-1, 0); len(ts) > limit {
			t.Fatalf("iter %d: %d transfers for %d nonzero balances (limit %d)", iter, len(ts), nonzero, limit)
		}
		if moved != credits {
			t.Fatalf("iter %d: moved %d, credits total %d", iter, moved, credits)
		}
		// 4. The order of the input does not matter.
		shuffled := slices.Clone(bs)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		again, err := ledger.SuggestTransfers(shuffled)
		if err != nil || !slices.Equal(ts, again) {
			t.Fatalf("iter %d: result depends on input order:\n%v\n%v", iter, ts, again)
		}
	}
}

// minTransfers is the exact optimum for small inputs: n minus the largest number
// of disjoint zero-sum groups the nonzero balances can be split into. Subset DP.
func minTransfers(bs []ledger.Balance) int {
	var nz []int64
	for _, b := range bs {
		if b.Amount != 0 {
			nz = append(nz, b.Amount)
		}
	}
	n := len(nz)
	if n == 0 {
		return 0
	}
	sum := make([]int64, 1<<n)
	for m := 1; m < 1<<n; m++ {
		low := 0
		for m>>low&1 == 0 {
			low++
		}
		sum[m] = sum[m&(m-1)] + nz[low]
	}
	best := make([]int, 1<<n)
	for m := range best {
		best[m] = -1
	}
	best[0] = 0
	for m := 1; m < 1<<n; m++ {
		for sub := m; sub > 0; sub = (sub - 1) & m {
			if sum[sub] == 0 && best[m^sub] >= 0 {
				best[m] = max(best[m], best[m^sub]+1)
			}
		}
	}
	return n - best[1<<n-1]
}

// The honest statement about this heuristic, as tests: it never beats the true
// minimum, it is sometimes worse, and it never exceeds n-1.
func TestSuggestTransfers_ReducesButDoesNotMinimize(t *testing.T) {
	// A concrete counterexample. Optimal: {-7,+7} and {-8,+2,+6} = 3 transfers.
	bs := []ledger.Balance{bal(1, -8), bal(2, -7), bal(3, 7), bal(4, 2), bal(5, 6)}
	if got := minTransfers(bs); got != 3 {
		t.Fatalf("exact optimum = %d, want 3", got)
	}
	ts, err := ledger.SuggestTransfers(bs)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 4 {
		t.Errorf("greedy used %d transfers, want 4 (one more than the optimum): %v", len(ts), ts)
	}

	// Across many random small inputs it is never below the optimum, and it is
	// strictly worse for some of them (so the doc must not say "minimizes").
	r := rand.New(rand.NewPCG(20261029, 4))
	worse := 0
	for range 3000 {
		bs := randBalances(r, 3+r.IntN(5))
		ts, err := ledger.SuggestTransfers(bs)
		if err != nil {
			t.Fatal(err)
		}
		opt := minTransfers(bs)
		if len(ts) < opt {
			t.Fatalf("greedy used %d transfers, fewer than the exact minimum %d: %v", len(ts), opt, bs)
		}
		if len(ts) > opt {
			worse++
		}
	}
	if worse == 0 {
		t.Error("expected the heuristic to be strictly worse than optimal on some inputs")
	}
	t.Logf("greedy exceeded the exact minimum in %d of 3000 random cases", worse)
}
