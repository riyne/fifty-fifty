package expenses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/riyne/fifty-fifty/internal/allocator"
	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/httpx"
	"github.com/riyne/fifty-fifty/internal/idempotency"
	"github.com/riyne/fifty-fifty/internal/ledger"
)

const (
	codeInvalidState   = "invalid_state"
	codeItemsUnclaimed = "items_unclaimed"
	maxSplitMembers    = 100
)

type finalizeRequest struct {
	SplitRestAmong []uuid.UUID `json:"split_rest_among"`
}

type shareJSON struct {
	MemberID      uuid.UUID `json:"member_id"`
	SubtotalMinor int64     `json:"subtotal_minor"`
	TaxMinor      int64     `json:"tax_minor"`
	TipMinor      int64     `json:"tip_minor"`
	TotalMinor    int64     `json:"total_minor"`
}

type finalizedJSON struct {
	expenseJSON
	LedgerTransactionID uuid.UUID   `json:"ledger_transaction_id"`
	Shares              []shareJSON `json:"shares"` // what each claimant owes for this expense, sorted by member id
}

// finalize turns a fully claimed expense into ledger entries. Payer only.
//
// Order of checks: path ids, then authorization WITHOUT a write lock (404 for a
// non-member, 403 for a member who is not the payer, so nobody else can hold up
// claimers by calling this), then the Idempotency-Key, the body, and the split
// list's membership (all 400s, none of which claim the key), then one
// idempotent transaction that takes FOR UPDATE on the expense row.
//
// FOR UPDATE waits for every in-flight claim (they hold FOR SHARE) and blocks new
// ones, so the claim set read below is stable until commit. A claim arriving
// during finalize waits, then sees `finalized` and gets 409.
func (h *Handler) finalize(w http.ResponseWriter, r *http.Request) {
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
	if e.PayerID != user.ID {
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "only the payer can finalize this expense")
		return
	}

	key, err := idempotency.ParseKey(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req finalizeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil && !errors.Is(err, httpx.ErrEmptyBody) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	split := slices.Clone(req.SplitRestAmong)
	slices.SortFunc(split, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	if len(split) > maxSplitMembers || len(slices.Compact(slices.Clone(split))) != len(split) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("split_rest_among must list each member at most once, at most %d members", maxSplitMembers))
		return
	}
	if len(split) > 0 {
		n, err := h.q.CountGroupMembers(r.Context(), db.CountGroupMembersParams{GroupID: groupID, MemberIds: split})
		if err != nil {
			h.internalError(w, "count split members", err)
			return
		}
		if int(n) != len(split) {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "split_rest_among must only list members of this group")
			return
		}
	}

	parts := make([]string, len(split))
	for i, id := range split {
		parts[i] = id.String()
	}
	fp := idempotency.Fingerprint("POST", "/v1/groups/{id}/expenses/{expenseId}/finalize", groupID.String(), expenseID.String(), strings.Join(parts, ","))
	out, err := idempotency.Do(r.Context(), h.pool, user.ID, key, fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return h.doFinalize(ctx, tx, user.ID, groupID, expenseID, split)
	})
	if errors.Is(err, idempotency.ErrKeyReuse) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "Idempotency-Key was already used with a different request")
		return
	}
	if err != nil {
		h.internalError(w, "finalize", err)
		return
	}
	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	_, _ = w.Write(out.Body)
}

// doFinalize runs inside the idempotency transaction. Every early return of a
// non-2xx result happens before anything is written, because that result is
// committed and replayed for the key.
func (h *Handler) doFinalize(ctx context.Context, tx pgx.Tx, payerID, groupID, expenseID uuid.UUID, split []uuid.UUID) (idempotency.Result, error) {
	q := h.q.WithTx(tx)

	locked, err := q.LockExpenseForFinalize(ctx, db.LockExpenseForFinalizeParams{UserID: payerID, ExpenseID: expenseID, GroupID: groupID})
	if errors.Is(err, pgx.ErrNoRows) {
		return jsonResult(http.StatusNotFound, errBody("not_found", "expense not found")), nil
	}
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("lock expense: %w", err)
	}
	if locked.Status != statusClaiming {
		return jsonResult(http.StatusConflict, errBody(codeInvalidState,
			"expense is "+locked.Status+"; only a claiming expense can be finalized")), nil
	}

	// The claim set is stable from here to commit.
	rows, err := q.ListExpenseItems(ctx, expenseID)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("list items: %w", err)
	}
	var unclaimed []uuid.UUID
	for _, row := range rows {
		if len(row.ClaimedBy) == 0 {
			unclaimed = append(unclaimed, row.ID)
		}
	}
	if len(unclaimed) > 0 {
		if len(split) == 0 {
			return jsonResult(http.StatusConflict, errBody(codeItemsUnclaimed,
				fmt.Sprintf("%d item(s) have no claimant; claim them or pass split_rest_among", len(unclaimed)))), nil
		}
		// "Split the rest evenly" becomes real claim rows, in this transaction, so
		// the ledger can always be recomputed from the claims table.
		if err := q.AddClaimsForItems(ctx, db.AddClaimsForItemsParams{ExpenseID: expenseID, GroupID: groupID, ItemIds: unclaimed, MemberIds: split}); err != nil {
			return idempotency.Result{}, fmt.Errorf("split the rest: %w", err)
		}
		if rows, err = q.ListExpenseItems(ctx, expenseID); err != nil {
			return idempotency.Result{}, fmt.Errorf("re-list items: %w", err)
		}
	}

	items := make([]allocator.Item, len(rows))
	for i, row := range rows {
		items[i] = allocator.Item{ID: row.ID, Price: row.PriceMinor, Claimants: row.ClaimedBy}
	}
	members, err := allocator.Allocate(items, locked.TaxMinor, locked.TipMinor)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("allocate: %w", err)
	}
	entries, err := ledger.ExpenseEntries(payerID, members)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("ledger entries: %w", err)
	}

	txID, err := q.CreateLedgerTransaction(ctx, db.CreateLedgerTransactionParams{GroupID: groupID, ExpenseID: expenseID, CreatedBy: payerID})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("create ledger transaction: %w", err)
	}
	if len(entries) > 0 { // the payer ate everything: a posting with no entries is valid
		p := db.CreateLedgerEntriesParams{TransactionID: txID, GroupID: groupID}
		for _, en := range entries {
			p.MemberIds = append(p.MemberIds, en.MemberID)
			p.Amounts = append(p.Amounts, en.Amount)
		}
		if err := q.CreateLedgerEntries(ctx, p); err != nil {
			return idempotency.Result{}, fmt.Errorf("create ledger entries: %w", err)
		}
	}
	if n, err := q.MarkExpenseFinalized(ctx, expenseID); err != nil || n != 1 {
		return idempotency.Result{}, fmt.Errorf("mark finalized: rows=%d err=%v", n, err)
	}

	var total int64
	shares := make([]shareJSON, len(members))
	for i, m := range members {
		total += m.Total
		shares[i] = shareJSON{MemberID: m.ID, SubtotalMinor: m.Subtotal, TaxMinor: m.Tax, TipMinor: m.Tip, TotalMinor: m.Total}
	}
	if err := h.audit(ctx, q, payerID, groupID, "expense.finalized", map[string]any{
		"expense_id": expenseID, "ledger_transaction_id": txID, "total_minor": total, "split_rest_items": len(unclaimed),
	}); err != nil {
		return idempotency.Result{}, fmt.Errorf("audit: %w", err)
	}

	view, err := q.GetExpenseForMember(ctx, db.GetExpenseForMemberParams{UserID: payerID, ExpenseID: expenseID, GroupID: groupID})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("re-read expense: %w", err)
	}
	return jsonResult(http.StatusOK, finalizedJSON{expenseJSON: expenseView(view, rows), LedgerTransactionID: txID, Shares: shares}), nil
}

func (h *Handler) audit(ctx context.Context, q *db.Queries, actor, group uuid.UUID, kind string, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return q.InsertAuditEvent(ctx, db.InsertAuditEventParams{ActorUserID: actor, GroupID: group, Kind: kind, Data: b})
}

func errBody(code, message string) map[string]string {
	return map[string]string{"code": code, "message": message}
}
