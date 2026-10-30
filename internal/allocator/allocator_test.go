package allocator_test

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/allocator"
)

// Property tests use a fixed seed so a failure reproduces; the seed and the
// iteration are in the failure message.
const seed = 20261029

func newRNG(t *testing.T) *rand.Rand {
	t.Helper()
	return rand.New(rand.NewPCG(seed, 1))
}

func randID(r *rand.Rand) uuid.UUID {
	var id uuid.UUID
	for i := range id {
		id[i] = byte(r.IntN(256))
	}
	return id
}

func randIDs(r *rand.Rand, n int) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for len(out) < n {
		id := randID(r)
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func sumShares(s []allocator.Share) (n int64) {
	for _, x := range s {
		n += x.Amount
	}
	return n
}

func sortedByID(ids []uuid.UUID) bool {
	return slices.IsSortedFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
}

// ---- Apportion ----

func TestApportion_KnownCases(t *testing.T) {
	a := uuid.UUID{1}
	b := uuid.UUID{2}
	c := uuid.UUID{3}

	// 100 / 3: the extra cent goes to the lowest id.
	got, err := allocator.SplitEvenly(100, []uuid.UUID{c, a, b})
	if err != nil {
		t.Fatal(err)
	}
	want := []allocator.Share{{ID: a, Amount: 34}, {ID: b, Amount: 33}, {ID: c, Amount: 33}}
	if !slices.Equal(got, want) {
		t.Errorf("100/3 = %v, want %v", got, want)
	}

	// 2 cents over 3 equal members: two lowest ids get one each.
	got, _ = allocator.SplitEvenly(2, []uuid.UUID{a, b, c})
	want = []allocator.Share{{ID: a, Amount: 1}, {ID: b, Amount: 1}, {ID: c, Amount: 0}}
	if !slices.Equal(got, want) {
		t.Errorf("2/3 = %v, want %v", got, want)
	}

	// Weights 1:2, total 10 -> exact 3.33 / 6.67. Remainders decide, not the id.
	got, _ = allocator.Apportion(10, []allocator.Weight{{ID: a, W: 1}, {ID: b, W: 2}})
	want = []allocator.Share{{ID: a, Amount: 3}, {ID: b, Amount: 7}}
	if !slices.Equal(got, want) {
		t.Errorf("10 @ 1:2 = %v, want %v", got, want)
	}

	// Zero weight member gets nothing even though they are lowest id.
	got, _ = allocator.Apportion(5, []allocator.Weight{{ID: a, W: 0}, {ID: b, W: 1}, {ID: c, W: 1}})
	want = []allocator.Share{{ID: a, Amount: 0}, {ID: b, Amount: 3}, {ID: c, Amount: 2}}
	if !slices.Equal(got, want) {
		t.Errorf("5 @ 0:1:1 = %v, want %v", got, want)
	}

	// Total 0 is fine even with no positive weight; no members and total 0 is empty.
	if got, err := allocator.Apportion(0, []allocator.Weight{{ID: a}}); err != nil || sumShares(got) != 0 {
		t.Errorf("0 over zero weights = %v, %v", got, err)
	}
	if got, err := allocator.Apportion(0, nil); err != nil || len(got) != 0 {
		t.Errorf("0 over nobody = %v, %v", got, err)
	}
}

func TestApportion_Errors(t *testing.T) {
	a, b := uuid.UUID{1}, uuid.UUID{2}
	tests := []struct {
		name  string
		total int64
		ws    []allocator.Weight
		want  error
	}{
		{"negative total", -1, []allocator.Weight{{ID: a, W: 1}}, allocator.ErrNegative},
		{"negative weight", 5, []allocator.Weight{{ID: a, W: -1}, {ID: b, W: 2}}, allocator.ErrNegative},
		{"duplicate id", 5, []allocator.Weight{{ID: a, W: 1}, {ID: a, W: 1}}, allocator.ErrDuplicate},
		{"no members", 5, nil, allocator.ErrNoWeight},
		{"all zero weights", 5, []allocator.Weight{{ID: a}, {ID: b}}, allocator.ErrNoWeight},
		{"weight sum overflows", 5, []allocator.Weight{{ID: a, W: math.MaxInt64}, {ID: b, W: 1}}, allocator.ErrOverflow},
	}
	for _, tc := range tests {
		if _, err := allocator.Apportion(tc.total, tc.ws); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestApportion_DoesNotMutateInput(t *testing.T) {
	r := newRNG(t)
	ids := randIDs(r, 5)
	ws := make([]allocator.Weight, len(ids))
	for i, id := range ids {
		ws[i] = allocator.Weight{ID: id, W: int64(i + 1)}
	}
	before := slices.Clone(ws)
	if _, err := allocator.Apportion(1000, ws); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ws, before) {
		t.Error("Apportion reordered or modified its input")
	}
}

// randWeights draws weights over a mix of magnitudes, including zeros, huge
// values (to exercise the 128-bit path) and ties.
func randWeights(r *rand.Rand, ids []uuid.UUID) ([]allocator.Weight, int64) {
	ws := make([]allocator.Weight, len(ids))
	var sum int64
	budget := int64(math.MaxInt64)
	for i, id := range ids {
		var w int64
		switch r.IntN(5) {
		case 0:
			w = 0
		case 1:
			w = 1
		case 2:
			w = int64(r.IntN(100))
		case 3:
			w = int64(r.IntN(1_000_000))
		default:
			w = r.Int64N(1 << 50)
		}
		if w > budget/int64(len(ids)) { // keep the sum from overflowing
			w = budget / int64(len(ids))
		}
		ws[i] = allocator.Weight{ID: id, W: w}
		sum += w
	}
	return ws, sum
}

func TestApportion_Properties(t *testing.T) {
	r := newRNG(t)
	for iter := range 20000 {
		n := 1 + r.IntN(8)
		ids := randIDs(r, n)
		ws, sum := randWeights(r, ids)

		var total int64
		switch r.IntN(4) {
		case 0:
			total = int64(r.IntN(10))
		case 1:
			total = int64(r.IntN(100_000))
		case 2:
			total = r.Int64N(1 << 40)
		default:
			total = r.Int64N(math.MaxInt64) // includes values where total*w needs 128 bits
		}

		got, err := allocator.Apportion(total, ws)
		if sum == 0 && total > 0 {
			if !errors.Is(err, allocator.ErrNoWeight) {
				t.Fatalf("iter %d: want ErrNoWeight, got %v", iter, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}

		// 1. Shares sum to exactly the total.
		if s := sumShares(got); s != total {
			t.Fatalf("iter %d (seed %d): shares sum to %d, want %d; ws=%v got=%v", iter, seed, s, total, ws, got)
		}
		// 2. Sorted by id, same members as input.
		gotIDs := make([]uuid.UUID, len(got))
		for i, s := range got {
			gotIDs[i] = s.ID
		}
		if !sortedByID(gotIDs) || len(got) != len(ws) {
			t.Fatalf("iter %d: result not sorted by id / wrong length", iter)
		}

		byID := map[uuid.UUID]int64{}
		for _, s := range got {
			byID[s.ID] = s.Amount
		}
		for _, w := range ws {
			// 3. Each share is floor(ideal) or floor(ideal)+1, checked against
			// an independent big.Int computation (no 128-bit tricks).
			floor := new(big.Int).Div(new(big.Int).Mul(big.NewInt(total), big.NewInt(w.W)), big.NewInt(max(sum, 1)))
			f := floor.Int64()
			if sum > 0 && (byID[w.ID] != f && byID[w.ID] != f+1) {
				t.Fatalf("iter %d: member share %d not in {%d, %d}; total=%d w=%d sum=%d",
					iter, byID[w.ID], f, f+1, total, w.W, sum)
			}
			// 4. Zero weight gets nothing.
			if w.W == 0 && byID[w.ID] != 0 {
				t.Fatalf("iter %d: zero-weight member got %d", iter, byID[w.ID])
			}
			// 5. Never negative.
			if byID[w.ID] < 0 {
				t.Fatalf("iter %d: negative share", iter)
			}
		}
		// 6. Monotone: a strictly larger weight never gets a smaller share.
		for _, x := range ws {
			for _, y := range ws {
				if x.W > y.W && byID[x.ID] < byID[y.ID] {
					t.Fatalf("iter %d: weight %d got %d but weight %d got %d", iter, x.W, byID[x.ID], y.W, byID[y.ID])
				}
			}
		}
		// 7. Order independence and determinism.
		shuffled := slices.Clone(ws)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		again, err := allocator.Apportion(total, shuffled)
		if err != nil || !slices.Equal(again, got) {
			t.Fatalf("iter %d: result depends on input order or is not deterministic:\n%v\n%v", iter, got, again)
		}
	}
}

// ---- Allocate ----

func TestAllocate_KnownCase(t *testing.T) {
	a, b, c := uuid.UUID{1}, uuid.UUID{2}, uuid.UUID{3}
	// Dinner: pizza 1500 shared by a,b,c; salad 700 a only; beer 801 b,c.
	// Subtotals: a = 500+700 = 1200; b = 500+401 = 901 (extra cent to b over c); c = 500+400 = 900.
	items := []allocator.Item{
		{Price: 1500, Claimants: []uuid.UUID{c, b, a}},
		{Price: 700, Claimants: []uuid.UUID{a}},
		{Price: 801, Claimants: []uuid.UUID{b, c}},
	}
	got, err := allocator.Allocate(items, 300, 450)
	if err != nil {
		t.Fatal(err)
	}
	var grand int64
	for _, m := range got {
		grand += m.Total
	}
	if grand != 1500+700+801+300+450 {
		t.Errorf("totals sum to %d, want %d", grand, 1500+700+801+300+450)
	}
	if got[0].ID != a || got[0].Subtotal != 1200 || got[1].Subtotal != 901 || got[2].Subtotal != 900 {
		t.Errorf("subtotals wrong: %+v", got)
	}
	// Tax 300 over weights 1200:901:900 (sum 3001). Ideals are 119.96, 90.07, 89.97, so the floors
	// are 119, 90, 89 (sum 298) and the 2 leftover cents go to the largest remainders: c (.97)
	// then a (.96), not to b (.07). Result 120, 90, 90.
	if got[0].Tax != 120 || got[1].Tax != 90 || got[2].Tax != 90 {
		t.Errorf("tax shares = %d,%d,%d want 120,90,90", got[0].Tax, got[1].Tax, got[2].Tax)
	}
}

func TestAllocate_Errors(t *testing.T) {
	a := uuid.UUID{1}
	ok := []allocator.Item{{Price: 100, Claimants: []uuid.UUID{a}}}
	if _, err := allocator.Allocate([]allocator.Item{{Price: 100}}, 0, 0); !errors.Is(err, allocator.ErrNoClaim) {
		t.Errorf("unclaimed item: %v", err)
	}
	if _, err := allocator.Allocate([]allocator.Item{{Price: -5, Claimants: []uuid.UUID{a}}}, 0, 0); !errors.Is(err, allocator.ErrNegative) {
		t.Errorf("negative price: %v", err)
	}
	if _, err := allocator.Allocate(ok, -1, 0); !errors.Is(err, allocator.ErrNegative) {
		t.Errorf("negative tax: %v", err)
	}
	if _, err := allocator.Allocate([]allocator.Item{{Price: 100, Claimants: []uuid.UUID{a, a}}}, 0, 0); !errors.Is(err, allocator.ErrDuplicate) {
		t.Errorf("duplicate claimant: %v", err)
	}
	if _, err := allocator.Allocate([]allocator.Item{{Price: 0, Claimants: []uuid.UUID{a}}}, 100, 0); !errors.Is(err, allocator.ErrNoWeight) {
		t.Errorf("tax with no positive subtotal: %v", err)
	}
	huge := []allocator.Item{
		{Price: math.MaxInt64, Claimants: []uuid.UUID{a}},
		{Price: 1, Claimants: []uuid.UUID{a}},
	}
	if _, err := allocator.Allocate(huge, 0, 0); !errors.Is(err, allocator.ErrOverflow) {
		t.Errorf("overflow: %v", err)
	}
	// No items, no tax, no tip: nothing to allocate, no error.
	if got, err := allocator.Allocate(nil, 0, 0); err != nil || len(got) != 0 {
		t.Errorf("empty expense = %v, %v", got, err)
	}
}

func TestAllocate_Properties(t *testing.T) {
	r := newRNG(t)
	for iter := range 20000 {
		members := randIDs(r, 1+r.IntN(6))
		items := make([]allocator.Item, 1+r.IntN(8))
		var itemSum int64
		var positive bool
		for i := range items {
			price := int64(r.IntN(5000))
			if r.IntN(6) == 0 {
				price = 0
			}
			k := 1 + r.IntN(len(members))
			cl := slices.Clone(members)
			r.Shuffle(len(cl), func(a, b int) { cl[a], cl[b] = cl[b], cl[a] })
			items[i] = allocator.Item{ID: uuid.UUID{byte(i)}, Price: price, Claimants: cl[:k]}
			itemSum += price
			positive = positive || price > 0
		}
		tax, tip := int64(r.IntN(2000)), int64(r.IntN(2000))
		if !positive {
			tax, tip = 0, 0
		}

		got, err := allocator.Allocate(items, tax, tip)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}

		// 1. Conservation: shares sum exactly to items + tax + tip.
		var grand, subs, taxes, tips int64
		for _, m := range got {
			grand += m.Total
			subs += m.Subtotal
			taxes += m.Tax
			tips += m.Tip
			if m.Total != m.Subtotal+m.Tax+m.Tip || m.Total < 0 {
				t.Fatalf("iter %d: bad member %+v", iter, m)
			}
		}
		if grand != itemSum+tax+tip || subs != itemSum || taxes != tax || tips != tip {
			t.Fatalf("iter %d (seed %d): conservation broken: grand=%d subs=%d tax=%d tip=%d; want %d/%d/%d/%d",
				iter, seed, grand, subs, taxes, tips, itemSum+tax+tip, itemSum, tax, tip)
		}

		// 2. Each member's subtotal is within (#items they share) of the exact
		// value. Compare in units of 1/lcm via big.Rat.
		for _, m := range got {
			exact := new(big.Rat)
			shared := 0
			for _, it := range items {
				if slices.Contains(it.Claimants, m.ID) {
					exact.Add(exact, big.NewRat(it.Price, int64(len(it.Claimants))))
					if len(it.Claimants) > 1 {
						shared++
					}
				}
			}
			diff := new(big.Rat).Sub(big.NewRat(m.Subtotal, 1), exact)
			bound := big.NewRat(int64(shared), 1)
			if new(big.Rat).Abs(diff).Cmp(bound) > 0 {
				t.Fatalf("iter %d: subtotal %d is %v from exact %v, bound %v", iter, m.Subtotal, diff, exact, bound)
			}
		}

		// 3. Members whose subtotal is 0 pay no tax or tip.
		for _, m := range got {
			if m.Subtotal == 0 && (m.Tax != 0 || m.Tip != 0) {
				t.Fatalf("iter %d: free-rider charged tax/tip: %+v", iter, m)
			}
		}

		// 4. Result does not depend on the order of items or claimants, and is
		// sorted by member id.
		ids := make([]uuid.UUID, len(got))
		for i, m := range got {
			ids[i] = m.ID
		}
		if !sortedByID(ids) {
			t.Fatalf("iter %d: result not sorted by id", iter)
		}
		shuffled := make([]allocator.Item, len(items))
		for i, it := range items {
			shuffled[i] = allocator.Item{ID: it.ID, Price: it.Price, Claimants: slices.Clone(it.Claimants)}
			cl := shuffled[i].Claimants
			r.Shuffle(len(cl), func(a, b int) { cl[a], cl[b] = cl[b], cl[a] })
		}
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		again, err := allocator.Allocate(shuffled, tax, tip)
		if err != nil || !slices.Equal(again, got) {
			t.Fatalf("iter %d: not order independent:\n%+v\n%+v", iter, got, again)
		}
	}
}

// A sole claimant pays the exact price of their item, with no rounding at all.
func TestAllocate_SoleClaimantExact(t *testing.T) {
	r := newRNG(t)
	for range 1000 {
		a := randID(r)
		price := int64(r.IntN(1_000_000))
		got, err := allocator.Allocate([]allocator.Item{{Price: price, Claimants: []uuid.UUID{a}}}, 0, 0)
		if err != nil || len(got) != 1 || got[0].Total != price {
			t.Fatalf("sole claimant: %+v, %v", got, err)
		}
	}
}
