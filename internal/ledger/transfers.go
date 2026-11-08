package ledger

import (
	"bytes"
	"errors"
	"math"
	"slices"

	"github.com/google/uuid"
)

// Balance is one member's net position in a group: positive means the group owes
// them, negative means they owe the group. A group's balances always sum to zero.
type Balance struct {
	MemberID uuid.UUID
	Amount   int64
}

// Transfer is a suggested payment: From pays To the amount.
type Transfer struct {
	From   uuid.UUID
	To     uuid.UUID
	Amount int64
}

var (
	ErrNotBalanced         = errors.New("ledger: balances do not sum to zero")
	ErrDuplicate           = errors.New("ledger: duplicate member id")
	ErrNegativeReservation = errors.New("ledger: a reservation is negative")
)

// withCapacity returns xs with each amount reduced by reserved(id), dropping those that reach
// zero or below.
func withCapacity(xs []Balance, reserved func(uuid.UUID) int64) []Balance {
	out := xs[:0]
	for _, x := range xs {
		if left := x.Amount - reserved(x.MemberID); left > 0 {
			out = append(out, Balance{MemberID: x.MemberID, Amount: left})
		}
	}
	return out
}

// Reserved is the money a member has tied up in open settlements (pending or unknown: asked
// of the payment provider, not yet in the ledger): what they are sending, and what they are
// to receive. It is what settlements.Available subtracts when a settlement is created.
type Reserved struct {
	Out int64 // the member is sending this much
	In  int64 // the member is to receive this much
}

// SuggestTransfers turns balances into a short list of payments that would bring
// every balance to zero.
//
// It is a greedy heuristic: repeatedly the member who is owed the most is paid by
// the member who owes the most, for the smaller of the two amounts. Each step
// settles at least one of them completely, so it never needs more than n-1
// transfers for n members with a nonzero balance, and it usually needs fewer than
// paying every debt separately. It does NOT find the minimum: finding it is the
// problem of splitting the members into as many independent zero-sum groups as
// possible, which is NP-hard in general. For example, balances -8, -7, +7, +2, +6
// can be settled in 3 transfers ({-7,+7} and {-8,+2,+6}); this function returns 4.
// Say "reduces", never "minimizes".
//
// Ties (equal amounts) go to the lowest member id, and the result does not depend
// on the order of the input. These are suggestions only: nothing is posted.
func SuggestTransfers(balances []Balance) ([]Transfer, error) {
	return SuggestTransfersAvailable(balances, nil)
}

// SuggestTransfersAvailable is SuggestTransfers for what can be settled RIGHT NOW: the
// payments that create-settlement would accept, given the money already reserved by open
// settlements. A member can send at most what they owe less what they are already sending
// (-balance - Out), and receive at most what they are owed less what they are already to
// receive (balance - In), the rule settlements.Available applies; a member with nothing left
// on their side is not matched. So every transfer returned fits both members' capacity, the
// transfers of one sender together fit theirs, and doing them all leaves nothing further to
// suggest. The balances are still checked to sum to zero (that is the ledger's own invariant,
// not affected by reservations); what cannot be matched, because the other side has no
// capacity left, is simply not suggested. reserved may be nil (nothing reserved), and a
// member missing from it has nothing reserved. A negative reservation is an error.
func SuggestTransfersAvailable(balances []Balance, reserved map[uuid.UUID]Reserved) ([]Transfer, error) {
	var sum int64
	creditors := make([]Balance, 0, len(balances))
	debtors := make([]Balance, 0, len(balances))
	seen := make(map[uuid.UUID]bool, len(balances))
	for _, b := range balances {
		if seen[b.MemberID] {
			return nil, ErrDuplicate
		}
		seen[b.MemberID] = true
		if (b.Amount > 0 && sum > math.MaxInt64-b.Amount) || (b.Amount < 0 && sum < math.MinInt64-b.Amount) {
			return nil, ErrOverflow
		}
		sum += b.Amount
		if r := reserved[b.MemberID]; r.Out < 0 || r.In < 0 {
			return nil, ErrNegativeReservation
		}
		switch {
		case b.Amount > 0:
			creditors = append(creditors, b)
		case b.Amount < 0:
			if b.Amount == math.MinInt64 {
				return nil, ErrOverflow // cannot be negated
			}
			debtors = append(debtors, Balance{MemberID: b.MemberID, Amount: -b.Amount}) // store what they owe, positive
		}
	}
	if sum != 0 {
		return nil, ErrNotBalanced
	}
	// What each side can still move: debtors can send what they owe less what they are already
	// sending; creditors can receive what they are owed less what they are already to receive.
	// Someone with nothing left drops out. (With no reservations this changes nothing.)
	debtors = withCapacity(debtors, func(id uuid.UUID) int64 { return reserved[id].Out })
	creditors = withCapacity(creditors, func(id uuid.UUID) int64 { return reserved[id].In })

	// Index of the largest remaining amount; the lowest id wins a tie.
	largest := func(xs []Balance) int {
		best := 0
		for i := 1; i < len(xs); i++ {
			if xs[i].Amount > xs[best].Amount ||
				(xs[i].Amount == xs[best].Amount && bytes.Compare(xs[i].MemberID[:], xs[best].MemberID[:]) < 0) {
				best = i
			}
		}
		return best
	}

	var out []Transfer
	// With nothing reserved debtors empties at the same moment as creditors (the balances sum
	// to zero); with reservations one side can run out first, and what is left of the other
	// side cannot be settled right now.
	for len(creditors) > 0 && len(debtors) > 0 {
		c, d := largest(creditors), largest(debtors)
		amount := min(creditors[c].Amount, debtors[d].Amount)
		out = append(out, Transfer{From: debtors[d].MemberID, To: creditors[c].MemberID, Amount: amount})
		creditors[c].Amount -= amount
		debtors[d].Amount -= amount
		if creditors[c].Amount == 0 {
			creditors = slices.Delete(creditors, c, c+1)
		}
		if debtors[d].Amount == 0 {
			debtors = slices.Delete(debtors, d, d+1)
		}
	}
	return out, nil
}
