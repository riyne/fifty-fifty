// Package allocator turns claims into exact per-member amounts.
//
// It is pure: no database, no clock, no randomness. Everything is int64 minor
// units (cents); there are no floats anywhere, so there is no rounding error to
// accumulate, only a remainder that is handed out explicitly.
//
// Two layers:
//
//   - Apportion splits a total in proportion to integer weights using the
//     largest-remainder method. Shares always sum to exactly the total.
//   - Allocate uses it for a whole expense: each item is split evenly among its
//     claimants, then tax and tip are prorated by each member's claimed subtotal.
//
// Determinism: the result depends only on the inputs as a set, never on the
// order they are passed in. Ties in the remainder are broken by member id
// (lowest first), and results come back sorted by member id.
//
// Known property, not a bug: with equal weights every item's leftover cent goes
// to the lowest-id claimant, so across k shared items one member can end up at
// most k cents above the exact value. Per item the error is always under one
// cent.
package allocator

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"

	"github.com/google/uuid"
)

var (
	ErrNegative  = errors.New("allocator: negative amount or weight")
	ErrDuplicate = errors.New("allocator: duplicate member id")
	ErrNoWeight  = errors.New("allocator: nothing to apportion over (no members, or all weights zero)")
	ErrOverflow  = errors.New("allocator: amount overflows int64")
	ErrNoClaim   = errors.New("allocator: item has no claimants")
)

// Weight is one member's proportional weight in an Apportion call.
type Weight struct {
	ID uuid.UUID
	W  int64
}

// Share is one member's amount.
type Share struct {
	ID     uuid.UUID
	Amount int64
}

func cmpID(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }

// Apportion splits total across the weights so that each share is either
// floor(total*w/sum) or one more, and the shares sum to exactly total.
// The extra units go to the largest fractional remainders; equal remainders
// go to the lowest member id. A member with weight 0 always gets 0.
//
// If total is 0 every share is 0 (even when all weights are zero). If total is
// positive there must be at least one positive weight.
//
// The result is sorted by member id.
func Apportion(total int64, weights []Weight) ([]Share, error) {
	if total < 0 {
		return nil, ErrNegative
	}
	ws := slices.Clone(weights)
	slices.SortFunc(ws, func(a, b Weight) int { return cmpID(a.ID, b.ID) })

	var sum int64
	for i, w := range ws {
		if w.W < 0 {
			return nil, ErrNegative
		}
		if i > 0 && ws[i-1].ID == w.ID {
			return nil, fmt.Errorf("%w: %s", ErrDuplicate, w.ID)
		}
		if sum > math.MaxInt64-w.W {
			return nil, ErrOverflow
		}
		sum += w.W
	}

	shares := make([]Share, len(ws))
	for i, w := range ws {
		shares[i].ID = w.ID
	}
	if total == 0 {
		return shares, nil
	}
	if sum == 0 {
		return nil, ErrNoWeight
	}

	// total*w can exceed 64 bits, so do the multiply and divide in 128 bits.
	// The quotient is at most total (w <= sum), so it fits and Div64 cannot
	// panic. rem < sum, and all remainders share the denominator sum, so they
	// compare exactly as plain integers.
	rems := make([]uint64, len(ws))
	var assigned int64
	for i, w := range ws {
		hi, lo := bits.Mul64(uint64(total), uint64(w.W))
		q, r := bits.Div64(hi, lo, uint64(sum))
		shares[i].Amount = int64(q)
		rems[i] = r
		assigned += int64(q)
	}

	// Hand the leftover units to the largest remainders. leftover is
	// (sum of remainders)/sum, and each remainder is < sum, so leftover is less
	// than the number of members with a nonzero remainder: a zero-weight member
	// (remainder 0) can never be picked.
	leftover := total - assigned
	order := make([]int, len(ws))
	for i := range order {
		order[i] = i
	}
	// ws is sorted by id, so a stable sort on remainder (descending) keeps
	// lower ids first among equal remainders.
	slices.SortStableFunc(order, func(a, b int) int {
		switch {
		case rems[a] > rems[b]:
			return -1
		case rems[a] < rems[b]:
			return 1
		}
		return 0
	})
	if leftover < 0 || leftover >= int64(len(ws)) {
		panic(fmt.Sprintf("allocator: leftover %d out of range for %d members", leftover, len(ws)))
	}
	for _, i := range order[:leftover] {
		shares[i].Amount++
	}
	return shares, nil
}

// SplitEvenly splits amount equally among ids, extra units to the lowest ids.
func SplitEvenly(amount int64, ids []uuid.UUID) ([]Share, error) {
	ws := make([]Weight, len(ids))
	for i, id := range ids {
		ws[i] = Weight{ID: id, W: 1}
	}
	return Apportion(amount, ws)
}

// Item is one line of an expense with the members who claimed it.
type Item struct {
	ID        uuid.UUID // only used in error messages
	Price     int64
	Claimants []uuid.UUID
}

// Member is one member's computed share of an expense.
// Total == Subtotal + Tax + Tip, and the Totals of all members sum to
// (sum of item prices) + tax + tip exactly.
type Member struct {
	ID       uuid.UUID
	Subtotal int64 // their part of the items
	Tax      int64 // their prorated part of tax
	Tip      int64 // their prorated part of tip
	Total    int64
}

// Allocate computes every claimant's share of an expense. Every item must have
// at least one claimant; the caller (finalize) is responsible for that, which
// is why it is an error here and not silently skipped.
//
// Members who claimed only zero-price items appear with Total 0.
// Tax and tip need at least one member with a positive subtotal.
// The result is sorted by member id.
func Allocate(items []Item, tax, tip int64) ([]Member, error) {
	if tax < 0 || tip < 0 {
		return nil, ErrNegative
	}

	subtotals := map[uuid.UUID]int64{}
	grand := tax
	if grand > math.MaxInt64-tip {
		return nil, ErrOverflow
	}
	grand += tip
	for _, it := range items {
		if it.Price < 0 {
			return nil, fmt.Errorf("item %s: %w", it.ID, ErrNegative)
		}
		if len(it.Claimants) == 0 {
			return nil, fmt.Errorf("item %s: %w", it.ID, ErrNoClaim)
		}
		if grand > math.MaxInt64-it.Price {
			return nil, ErrOverflow
		}
		grand += it.Price
		shares, err := SplitEvenly(it.Price, it.Claimants)
		if err != nil {
			return nil, fmt.Errorf("item %s: %w", it.ID, err)
		}
		for _, s := range shares {
			subtotals[s.ID] += s.Amount // bounded by grand, which did not overflow
		}
	}

	weights := make([]Weight, 0, len(subtotals))
	for id, sub := range subtotals {
		weights = append(weights, Weight{ID: id, W: sub})
	}
	taxShares, err := Apportion(tax, weights)
	if err != nil {
		return nil, fmt.Errorf("tax: %w", err)
	}
	tipShares, err := Apportion(tip, weights)
	if err != nil {
		return nil, fmt.Errorf("tip: %w", err)
	}

	// Apportion returns shares sorted by id for the same id set, so the three
	// slices line up index for index.
	out := make([]Member, len(taxShares))
	for i := range out {
		id := taxShares[i].ID
		m := Member{ID: id, Subtotal: subtotals[id], Tax: taxShares[i].Amount, Tip: tipShares[i].Amount}
		m.Total = m.Subtotal + m.Tax + m.Tip
		out[i] = m
	}
	return out, nil
}
