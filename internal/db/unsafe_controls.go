//go:build unsafe_controls

// Hand-written, NOT sqlc output (sqlc generates only the *.sql.go files and models.go
// here). These are the negative-control variants of the queries that carry a lock or
// a guard. Each is derived from the generated SQL text by cutting out exactly the
// lock clause or status condition, so it cannot drift from the real query, and the
// derivation panics at start-up if the clause is not found (which would mean the
// query changed and the control no longer removes what it claims to).
//
// Compiled only with the unsafe_controls tag; see internal/controls.
package db

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

func without(sql, clause string) string {
	if strings.Count(sql, clause) != 1 {
		panic("db: unsafe control: clause " + clause + " not found exactly once in: " + sql)
	}
	return strings.Replace(sql, clause, "", 1)
}

var (
	unsafeLockExpenseForClaim    = without(lockExpenseForClaim, "FOR SHARE OF e")
	unsafeLockExpenseForFinalize = without(lockExpenseForFinalize, "FOR UPDATE OF e")
	unsafeLockExpenseForVoid     = without(lockExpenseForVoid, "FOR UPDATE OF e")
	unsafeLockExpenseForEdit     = without(lockExpenseForEdit, "FOR NO KEY UPDATE OF e")
	unsafeLockGroupForSettlement = without(lockGroupForSettlement, "FOR NO KEY UPDATE")
	unsafeLockSettlement         = without(lockSettlement, "FOR UPDATE")

	unsafeMarkSettlementSucceeded = without(markSettlementSucceeded, " AND status IN ('pending', 'unknown')")
	unsafeMarkSettlementFailed    = without(markSettlementFailed, " AND status IN ('pending', 'unknown')")
	unsafeMarkSettlementUnknown   = without(markSettlementUnknown, " AND status = 'pending'")
)

// UnsafeLockExpenseForClaim is LockExpenseForClaim without FOR SHARE.
func (q *Queries) UnsafeLockExpenseForClaim(ctx context.Context, arg LockExpenseForClaimParams) (string, error) {
	var status string
	err := q.db.QueryRow(ctx, unsafeLockExpenseForClaim, arg.UserID, arg.ExpenseID, arg.GroupID).Scan(&status)
	return status, err
}

// UnsafeLockExpenseForFinalize is LockExpenseForFinalize without FOR UPDATE.
func (q *Queries) UnsafeLockExpenseForFinalize(ctx context.Context, arg LockExpenseForFinalizeParams) (LockExpenseForFinalizeRow, error) {
	var i LockExpenseForFinalizeRow
	err := q.db.QueryRow(ctx, unsafeLockExpenseForFinalize, arg.ExpenseID, arg.GroupID, arg.UserID).Scan(&i.Status, &i.TaxMinor, &i.TipMinor)
	return i, err
}

// UnsafeLockExpenseForVoid is LockExpenseForVoid without FOR UPDATE.
func (q *Queries) UnsafeLockExpenseForVoid(ctx context.Context, arg LockExpenseForVoidParams) (string, error) {
	var status string
	err := q.db.QueryRow(ctx, unsafeLockExpenseForVoid, arg.UserID, arg.ExpenseID, arg.GroupID).Scan(&status)
	return status, err
}

// UnsafeLockExpenseForEdit is LockExpenseForEdit without FOR NO KEY UPDATE.
func (q *Queries) UnsafeLockExpenseForEdit(ctx context.Context, arg LockExpenseForEditParams) (LockExpenseForEditRow, error) {
	var i LockExpenseForEditRow
	err := q.db.QueryRow(ctx, unsafeLockExpenseForEdit, arg.ExpenseID, arg.GroupID, arg.UserID).Scan(&i.Status, &i.Version, &i.TaxMinor, &i.TipMinor)
	return i, err
}

// UnsafeLockGroupForSettlement is LockGroupForSettlement without FOR NO KEY UPDATE.
func (q *Queries) UnsafeLockGroupForSettlement(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var got uuid.UUID
	err := q.db.QueryRow(ctx, unsafeLockGroupForSettlement, id).Scan(&got)
	return got, err
}

// UnsafeLockSettlement is LockSettlement without FOR UPDATE.
func (q *Queries) UnsafeLockSettlement(ctx context.Context, id uuid.UUID) (LockSettlementRow, error) {
	var i LockSettlementRow
	err := q.db.QueryRow(ctx, unsafeLockSettlement, id).Scan(&i.ID, &i.GroupID, &i.FromMember, &i.ToMember, &i.AmountMinor, &i.Status)
	return i, err
}

// UnsafeMarkSettlementSucceeded is MarkSettlementSucceeded without its status condition.
func (q *Queries) UnsafeMarkSettlementSucceeded(ctx context.Context, arg MarkSettlementSucceededParams) (int64, error) {
	r, err := q.db.Exec(ctx, unsafeMarkSettlementSucceeded, arg.LedgerTransactionID, arg.ID)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected(), nil
}

// UnsafeMarkSettlementFailed is MarkSettlementFailed without its status condition.
func (q *Queries) UnsafeMarkSettlementFailed(ctx context.Context, arg MarkSettlementFailedParams) (int64, error) {
	r, err := q.db.Exec(ctx, unsafeMarkSettlementFailed, arg.FailureCode, arg.ID)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected(), nil
}

// UnsafeMarkSettlementUnknown is MarkSettlementUnknown without its status condition.
func (q *Queries) UnsafeMarkSettlementUnknown(ctx context.Context, id uuid.UUID) (int64, error) {
	r, err := q.db.Exec(ctx, unsafeMarkSettlementUnknown, id)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected(), nil
}
