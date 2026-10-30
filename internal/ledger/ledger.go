// Package ledger holds the pure bookkeeping rules: turning per-member shares
// into ledger entries. No database, no clock.
//
// Entries are signed net amounts per member in minor units. Positive means the
// group owes that member; negative means they owe the group. Every posting sums
// to exactly zero, which the database also enforces (migration 00005).
package ledger

import (
	"bytes"
	"errors"
	"math"
	"slices"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/allocator"
)

// Entry is one member's net amount in a posting.
type Entry struct {
	MemberID uuid.UUID
	Amount   int64
}

var (
	ErrNegativeShare = errors.New("ledger: negative share")
	ErrOverflow      = errors.New("ledger: total overflows int64")
)

// ExpenseEntries returns the postings for a finalized expense: the payer is
// credited what everyone else owes, and every other member with a nonzero share
// is debited that share. A payer who also claimed items is only credited the
// difference (their own share is never both credited and debited). Zero entries
// are omitted, so an expense the payer fully ate up posts no entries at all.
//
// shares is allocator.Allocate's output. The payer need not appear in it.
// The result is sorted by member id.
func ExpenseEntries(payer uuid.UUID, shares []allocator.Member) ([]Entry, error) {
	var owedByOthers int64
	var entries []Entry
	for _, s := range shares {
		if s.Total < 0 {
			return nil, ErrNegativeShare
		}
		if s.ID == payer || s.Total == 0 {
			continue
		}
		if owedByOthers > math.MaxInt64-s.Total {
			return nil, ErrOverflow
		}
		owedByOthers += s.Total
		entries = append(entries, Entry{MemberID: s.ID, Amount: -s.Total})
	}
	if owedByOthers > 0 {
		entries = append(entries, Entry{MemberID: payer, Amount: owedByOthers})
	}
	slices.SortFunc(entries, func(a, b Entry) int { return bytes.Compare(a.MemberID[:], b.MemberID[:]) })
	return entries, nil
}
