package ledger_test

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/ledger"
)

func res(m map[byte]ledger.Reserved) map[uuid.UUID]ledger.Reserved {
	out := map[uuid.UUID]ledger.Reserved{}
	for k, v := range m {
		out[id(k)] = v
	}
	return out
}

func TestSuggestTransfersAvailable_KnownCases(t *testing.T) {
	// Balances: 1 is owed 5000; 2 owes 3000; 3 owes 2000.
	base := []ledger.Balance{bal(1, 5000), bal(2, -3000), bal(3, -2000)}
	tests := []struct {
		name string
		in   []ledger.Balance
		res  map[byte]ledger.Reserved
		want []ledger.Transfer
	}{
		{"nothing reserved is the plain suggestion", base, nil,
			[]ledger.Transfer{{id(2), id(1), 3000}, {id(3), id(1), 2000}}},
		{"a debtor's open settlement leaves less to suggest", base, map[byte]ledger.Reserved{2: {Out: 1000}},
			[]ledger.Transfer{{id(2), id(1), 2000}, {id(3), id(1), 2000}}},
		{"a debtor with everything reserved is not suggested at all", base, map[byte]ledger.Reserved{2: {Out: 3000}},
			[]ledger.Transfer{{id(3), id(1), 2000}}},
		{"the creditor's incoming money is counted: it can receive less", base, map[byte]ledger.Reserved{1: {In: 4000}},
			[]ledger.Transfer{{id(2), id(1), 1000}}},
		{"the creditor cannot receive any more: nothing to suggest", base, map[byte]ledger.Reserved{1: {In: 5000}}, nil},
		{"both sides reserved", base, map[byte]ledger.Reserved{2: {Out: 500}, 1: {In: 1000}},
			[]ledger.Transfer{{id(2), id(1), 2500}, {id(3), id(1), 1500}}},
		// A sender's capacity is not reduced by what they are to receive, nor a payee's by what
		// they are sending: only Out counts for a sender and only In for a payee (Available).
		{"a debtor's incoming reservation does not matter to what they can send", base, map[byte]ledger.Reserved{2: {In: 9999}},
			[]ledger.Transfer{{id(2), id(1), 3000}, {id(3), id(1), 2000}}},
		{"a creditor's outgoing reservation does not matter to what they can receive", base, map[byte]ledger.Reserved{1: {Out: 9999}},
			[]ledger.Transfer{{id(2), id(1), 3000}, {id(3), id(1), 2000}}},
		{"more reserved than owed is the same as all of it", base, map[byte]ledger.Reserved{2: {Out: 99999}},
			[]ledger.Transfer{{id(3), id(1), 2000}}},
		{"settled group", []ledger.Balance{bal(1, 0), bal(2, 0)}, map[byte]ledger.Reserved{1: {Out: 5}}, nil},
	}
	for _, tc := range tests {
		got, err := ledger.SuggestTransfersAvailable(tc.in, res(tc.res))
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, %v, want %v", tc.name, got, err, tc.want)
		}
	}
}

func TestSuggestTransfersAvailable_Errors(t *testing.T) {
	if _, err := ledger.SuggestTransfersAvailable([]ledger.Balance{bal(1, 5), bal(2, -5)}, res(map[byte]ledger.Reserved{2: {Out: -1}})); !errors.Is(err, ledger.ErrNegativeReservation) {
		t.Errorf("negative Out: %v", err)
	}
	if _, err := ledger.SuggestTransfersAvailable([]ledger.Balance{bal(1, 5), bal(2, -5)}, res(map[byte]ledger.Reserved{1: {In: -1}})); !errors.Is(err, ledger.ErrNegativeReservation) {
		t.Errorf("negative In: %v", err)
	}
	// Reservations do not repair or hide a ledger that does not balance.
	if _, err := ledger.SuggestTransfersAvailable([]ledger.Balance{bal(1, 5), bal(2, -4)}, res(map[byte]ledger.Reserved{2: {Out: 4}})); !errors.Is(err, ledger.ErrNotBalanced) {
		t.Errorf("unbalanced: %v", err)
	}
	if _, err := ledger.SuggestTransfersAvailable([]ledger.Balance{bal(1, 5), bal(1, -5)}, nil); !errors.Is(err, ledger.ErrDuplicate) {
		t.Errorf("duplicate: %v", err)
	}
}

func randomBalances(rng *rand.Rand, n int) []ledger.Balance {
	bs := make([]ledger.Balance, n)
	var sum int64
	for i := 0; i < n-1; i++ {
		bs[i] = bal(byte(i+1), rng.Int64N(20001)-10000)
		sum += bs[i].Amount
	}
	bs[n-1] = bal(byte(n), -sum)
	return bs
}

// With nothing reserved it is exactly SuggestTransfers.
func TestSuggestTransfersAvailable_NoReservationsEqualsSuggestTransfers(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		bs := randomBalances(rng, 2+rng.IntN(9))
		want, err1 := ledger.SuggestTransfers(bs)
		got, err2 := ledger.SuggestTransfersAvailable(bs, nil)
		got2, err3 := ledger.SuggestTransfersAvailable(bs, map[uuid.UUID]ledger.Reserved{})
		if err1 != nil || err2 != nil || err3 != nil || !slices.Equal(got, want) || !slices.Equal(got2, want) {
			t.Fatalf("balances %v: SuggestTransfers %v, Available(nil) %v, Available({}) %v (%v %v %v)", bs, want, got, got2, err1, err2, err3)
		}
	}
}

// The properties that make a suggestion safe to act on: whatever is reserved, every
// transfer fits the sender's and the payee's capacity by the rule create-settlement uses,
// the transfers of one member together fit it, doing them all leaves nothing more to
// suggest, and the result does not depend on the order of the input.
func TestSuggestTransfersAvailable_Properties(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for iter := range 3000 {
		n := 2 + rng.IntN(9)
		bs := randomBalances(rng, n)
		r := map[uuid.UUID]ledger.Reserved{}
		for _, b := range bs {
			if rng.IntN(2) == 0 {
				r[b.MemberID] = ledger.Reserved{Out: rng.Int64N(12001), In: rng.Int64N(12001)}
			}
		}
		got, err := ledger.SuggestTransfersAvailable(bs, r)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		balance := map[uuid.UUID]int64{}
		for _, b := range bs {
			balance[b.MemberID] = b.Amount
		}
		sent, received := map[uuid.UUID]int64{}, map[uuid.UUID]int64{}
		for _, tr := range got {
			if tr.Amount <= 0 || tr.From == tr.To {
				t.Fatalf("iter %d: a transfer of %d from %v to %v", iter, tr.Amount, tr.From, tr.To)
			}
			sent[tr.From] += tr.Amount
			received[tr.To] += tr.Amount
			// settlements.Available for this pair on its own.
			canSend := -balance[tr.From] - r[tr.From].Out
			canReceive := balance[tr.To] - r[tr.To].In
			if tr.Amount > min(canSend, canReceive) {
				t.Fatalf("iter %d: %d from %v to %v exceeds what create-settlement allows (send %d, receive %d)\nbalances %v reserved %v", iter, tr.Amount, tr.From, tr.To, canSend, canReceive, bs, r)
			}
		}
		for m, s := range sent { // together, as they are created one after another
			if s > -balance[m]-r[m].Out {
				t.Fatalf("iter %d: %v is asked to send %d in all, capacity %d", iter, m, s, -balance[m]-r[m].Out)
			}
		}
		for m, s := range received {
			if s > balance[m]-r[m].In {
				t.Fatalf("iter %d: %v is to receive %d in all, capacity %d", iter, m, s, balance[m]-r[m].In)
			}
		}
		// Do them all: the money is now reserved, and nothing is left to suggest.
		r2 := map[uuid.UUID]ledger.Reserved{}
		for m, v := range r {
			r2[m] = v
		}
		for m, s := range sent {
			v := r2[m]
			v.Out += s
			r2[m] = v
		}
		for m, s := range received {
			v := r2[m]
			v.In += s
			r2[m] = v
		}
		if again, err := ledger.SuggestTransfersAvailable(bs, r2); err != nil || len(again) != 0 {
			t.Fatalf("iter %d: after creating every suggestion, %v is still suggested (%v)\nbalances %v reserved %v", iter, again, err, bs, r)
		}
		// Order of the input does not matter.
		shuffled := slices.Clone(bs)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if other, err := ledger.SuggestTransfersAvailable(shuffled, r); err != nil || !slices.Equal(other, got) {
			t.Fatalf("iter %d: the result depends on the input order: %v against %v", iter, other, got)
		}
	}
}
