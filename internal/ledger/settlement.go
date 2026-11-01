package ledger

import (
	"bytes"
	"errors"
	"slices"

	"github.com/google/uuid"
)

// ErrBadSettlement means the sender, payee or amount cannot make a posting.
var ErrBadSettlement = errors.New("ledger: settlement needs two different members and a positive amount")

// SettlementEntries returns the postings for a successful settlement: the sender
// (who owes, so their balance is negative) is credited the amount and the payee
// (who is owed) is debited it, so each moves toward zero and the pair sums to
// zero. The result is sorted by member id.
func SettlementEntries(from, to uuid.UUID, amount int64) ([]Entry, error) {
	if from == to || amount <= 0 {
		return nil, ErrBadSettlement
	}
	entries := []Entry{{MemberID: from, Amount: amount}, {MemberID: to, Amount: -amount}}
	slices.SortFunc(entries, func(a, b Entry) int { return bytes.Compare(a.MemberID[:], b.MemberID[:]) })
	return entries, nil
}
