//go:build !unsafe_controls

// Package controls is the seam for the negative controls (DESIGN section 5, "Negative
// controls"). Each function below is the one place the code takes a lock or checks a
// guard that a negative-control run must be able to switch off.
//
// This file is the production build: every function is a plain call to the sqlc query
// it wraps, and Disabled is constant false. There is no flag, environment variable or
// other way to change that at runtime. The unsafe variants exist only in
// controls_unsafe.go and internal/db/unsafe_controls.go, both behind the
// unsafe_controls build tag, so they are not compiled into cmd/api or cmd/worker. A
// test builds those binaries and fails if any of that code is in them.
package controls

import (
	"context"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/db"
)

// Disabled reports whether c is switched off. Always false in this build.
func Disabled(Control) bool { return false }

func LockExpenseForClaim(ctx context.Context, q *db.Queries, arg db.LockExpenseForClaimParams) (string, error) {
	return q.LockExpenseForClaim(ctx, arg)
}

func LockExpenseForFinalize(ctx context.Context, q *db.Queries, arg db.LockExpenseForFinalizeParams) (db.LockExpenseForFinalizeRow, error) {
	return q.LockExpenseForFinalize(ctx, arg)
}

func LockExpenseForVoid(ctx context.Context, q *db.Queries, arg db.LockExpenseForVoidParams) (string, error) {
	return q.LockExpenseForVoid(ctx, arg)
}

func LockExpenseForEdit(ctx context.Context, q *db.Queries, arg db.LockExpenseForEditParams) (db.LockExpenseForEditRow, error) {
	return q.LockExpenseForEdit(ctx, arg)
}

// VersionMatches reports whether the version the client sent is the expense's current
// version. It is the optimistic lock of item edits and confirm.
func VersionMatches(current, sent int32) bool { return current == sent }

func LockGroupForSettlement(ctx context.Context, q *db.Queries, id uuid.UUID) (uuid.UUID, error) {
	return q.LockGroupForSettlement(ctx, id)
}

func LockSettlement(ctx context.Context, q *db.Queries, id uuid.UUID) (db.LockSettlementRow, error) {
	return q.LockSettlement(ctx, id)
}

func MarkSettlementSucceeded(ctx context.Context, q *db.Queries, arg db.MarkSettlementSucceededParams) (int64, error) {
	return q.MarkSettlementSucceeded(ctx, arg)
}

func MarkSettlementFailed(ctx context.Context, q *db.Queries, arg db.MarkSettlementFailedParams) (int64, error) {
	return q.MarkSettlementFailed(ctx, arg)
}

func MarkSettlementUnknown(ctx context.Context, q *db.Queries, id uuid.UUID) (int64, error) {
	return q.MarkSettlementUnknown(ctx, id)
}
