package expenses

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/controls"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/httpx"
	"github.com/riyne/fifty-fifty/internal/idempotency"
)

const statusFinalized = "finalized"

type voidedJSON struct {
	expenseJSON
	LedgerTransactionID         uuid.UUID `json:"ledger_transaction_id"`          // the new reversing posting
	ReversedLedgerTransactionID uuid.UUID `json:"reversed_ledger_transaction_id"` // the finalize posting it cancels
}

// void cancels a finalized expense by appending a reversing posting. Payer or
// group owner. The original ledger rows are never touched: the ledger is
// append-only, so a void is a second transaction whose entries are the exact
// negation of the first, and every balance returns to what it would have been
// had the expense never been finalized. Claims are left as they were (they are
// the record of who had what).
//
// Same shape as finalize: authorization without a write lock (404 non-member,
// 403 member who is neither payer nor owner), then the Idempotency-Key, then one
// idempotent transaction that takes FOR UPDATE on the expense row.
//
// Only a finalized expense can be voided: a claiming expense has nothing on the
// ledger to reverse, and the schema requires a void expense to have been
// finalized first.
func (h *Handler) void(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	groupID, err1 := uuid.Parse(r.PathValue("id"))
	expenseID, err2 := uuid.Parse(r.PathValue("expenseId"))
	if err1 != nil || err2 != nil {
		expenseNotFound(w)
		return
	}
	e, err := h.q.GetExpenseForMember(r.Context(), db.GetExpenseForMemberParams{UserID: user.ID, ExpenseID: expenseID, GroupID: groupID})
	if errors.Is(err, pgx.ErrNoRows) {
		expenseNotFound(w)
		return
	}
	if err != nil {
		h.internalError(w, "get expense", err)
		return
	}
	if e.PayerID != user.ID && e.CallerRole != "owner" {
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "only the payer or the group owner can void this expense")
		return
	}

	key, err := idempotency.ParseKey(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req struct{} // no body is expected; `{}` is accepted, anything else is rejected
	if err := httpx.DecodeJSON(w, r, &req); err != nil && !errors.Is(err, httpx.ErrEmptyBody) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	fp := idempotency.Fingerprint("POST", "/v1/groups/{id}/expenses/{expenseId}/void", groupID.String(), expenseID.String())
	out, err := idempotency.Do(r.Context(), h.pool, user.ID, key, fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return h.doVoid(ctx, tx, user.ID, e.CallerRole, groupID, expenseID)
	})
	if errors.Is(err, idempotency.ErrKeyReuse) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "Idempotency-Key was already used with a different request")
		return
	}
	if err != nil {
		h.internalError(w, "void", err)
		return
	}
	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	_, _ = w.Write(out.Body)
}

// doVoid runs inside the idempotency transaction. Every non-2xx result is
// returned before anything is written, because that result is committed and
// replayed for the key.
func (h *Handler) doVoid(ctx context.Context, tx pgx.Tx, callerID uuid.UUID, callerRole string, groupID, expenseID uuid.UUID) (idempotency.Result, error) {
	q := h.q.WithTx(tx)

	status, err := controls.LockExpenseForVoid(ctx, q, db.LockExpenseForVoidParams{UserID: callerID, ExpenseID: expenseID, GroupID: groupID})
	if errors.Is(err, pgx.ErrNoRows) {
		return jsonResult(http.StatusNotFound, errBody("not_found", "expense not found")), nil
	}
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("lock expense: %w", err)
	}
	if status != statusFinalized {
		return jsonResult(http.StatusConflict, errBody(codeInvalidState,
			"expense is "+status+"; only a finalized expense can be voided")), nil
	}

	original, err := q.GetExpensePosting(ctx, expenseID)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("find the finalize posting: %w", err)
	}
	voidTx, err := q.CreateVoidTransaction(ctx, db.CreateVoidTransactionParams{GroupID: groupID, ExpenseID: expenseID, ReversesID: original, CreatedBy: callerID})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("create void transaction: %w", err)
	}
	reversed, err := q.ReverseLedgerEntries(ctx, db.ReverseLedgerEntriesParams{VoidTransactionID: voidTx, OriginalTransactionID: original})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("reverse entries: %w", err)
	}
	if n, err := q.MarkExpenseVoid(ctx, expenseID); err != nil || n != 1 {
		return idempotency.Result{}, fmt.Errorf("mark void: rows=%d err=%v", n, err)
	}
	if err := h.audit(ctx, q, callerID, groupID, "expense.voided", map[string]any{
		"expense_id": expenseID, "ledger_transaction_id": voidTx, "reverses_ledger_transaction_id": original,
		"reversed_entries": reversed, "voided_as": voidedAs(callerRole),
	}); err != nil {
		return idempotency.Result{}, fmt.Errorf("audit: %w", err)
	}

	view, err := q.GetExpenseForMember(ctx, db.GetExpenseForMemberParams{UserID: callerID, ExpenseID: expenseID, GroupID: groupID})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("re-read expense: %w", err)
	}
	rows, err := q.ListExpenseItems(ctx, expenseID)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("list items: %w", err)
	}
	return jsonResult(http.StatusOK, voidedJSON{expenseJSON: expenseView(view, rows), LedgerTransactionID: voidTx, ReversedLedgerTransactionID: original}), nil
}

// voidedAs records which of the caller's capacities allowed the void, for the
// audit trail. An owner who is also the payer is recorded as the owner.
func voidedAs(role string) string {
	if role == "owner" {
		return "owner"
	}
	return "payer"
}
