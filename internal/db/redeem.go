package db

import (
	"context"

	"github.com/google/uuid"
)

// RedeemedJoinCode is what redeem_join_code (migration 00016) returns for a usable code.
type RedeemedJoinCode struct {
	GroupID    uuid.UUID
	JoinCodeID uuid.UUID
	// Added is false when the caller was already a member: nothing was spent.
	Added bool
}

// RedeemJoinCode is the only way fifty_app becomes a member of a group it did not create.
// The function locks a usable code, adds the caller (the transaction's app.user_id; there
// is no way to pass someone else) unless already a member, and spends a use only when it
// added one. pgx.ErrNoRows means the code is not found, revoked, expired or exhausted,
// deliberately indistinguishable.
//
// It is written by hand, not generated: sqlc cannot read the columns of a table-returning
// plpgsql function. The query is a constant, with one bound parameter.
func (q *Queries) RedeemJoinCode(ctx context.Context, codeHash []byte) (RedeemedJoinCode, error) {
	var r RedeemedJoinCode
	err := q.db.QueryRow(ctx, `SELECT group_id, join_code_id, added FROM redeem_join_code($1)`, codeHash).
		Scan(&r.GroupID, &r.JoinCodeID, &r.Added)
	return r, err
}
