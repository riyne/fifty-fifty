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
	ErrNotBalanced = errors.New("ledger: balances do not sum to zero")
	ErrDuplicate   = errors.New("ledger: duplicate member id")
)

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
	for len(creditors) > 0 { // debtors empties at the same moment: the balances sum to zero
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
