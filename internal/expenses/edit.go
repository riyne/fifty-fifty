package expenses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/httpx"
	"github.com/riyne/fifty-fifty/internal/idempotency"
)

const (
	statusNeedsReview = "needs_review"

	codeVersionConflict = "version_conflict"
	codeItemNotFound    = "item_not_found"
)

// ---- item edits ----

type itemUpdate struct {
	ItemID      uuid.UUID `json:"item_id"`
	Description *string   `json:"description"`
	PriceMinor  *int64    `json:"price_minor"`
}

type editRequest struct {
	Version  *int32        `json:"version"` // pointer: a missing version is an error
	Update   []itemUpdate  `json:"update"`
	Remove   []uuid.UUID   `json:"remove"`
	Add      []itemRequest `json:"add"`
	TaxMinor *int64        `json:"tax_minor"`
	TipMinor *int64        `json:"tip_minor"`
}

// normEdit is a validated edit with text trimmed and the order-insensitive lists
// sorted. It is also what gets fingerprinted. Add keeps its order (it decides the
// new items' positions).
type normEdit struct {
	Version  int32        `json:"version"`
	Update   []normUpdate `json:"update"`
	Remove   []uuid.UUID  `json:"remove"`
	Add      []normItem   `json:"add"`
	TaxMinor *int64       `json:"tax_minor"`
	TipMinor *int64       `json:"tip_minor"`
}

type normUpdate struct {
	ItemID      uuid.UUID `json:"item_id"`
	Description *string   `json:"description"`
	PriceMinor  *int64    `json:"price_minor"`
}

func cmpUUID(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }

func (req editRequest) normalize() (normEdit, error) {
	n := normEdit{Update: []normUpdate{}, Remove: []uuid.UUID{}, Add: []normItem{}}
	if req.Version == nil || *req.Version < 1 {
		return n, errors.New("version is required (the version of the expense you are editing)")
	}
	n.Version = *req.Version
	if len(req.Update) > maxItems || len(req.Remove) > maxItems || len(req.Add) > maxItems {
		return n, fmt.Errorf("update, remove and add each take at most %d entries", maxItems)
	}
	if len(req.Update) == 0 && len(req.Remove) == 0 && len(req.Add) == 0 && req.TaxMinor == nil && req.TipMinor == nil {
		return n, errors.New("nothing to change")
	}

	seen := map[uuid.UUID]bool{}
	for i, u := range req.Update {
		if u.ItemID == uuid.Nil {
			return n, fmt.Errorf("update[%d].item_id is required", i)
		}
		if seen[u.ItemID] {
			return n, fmt.Errorf("item %s appears more than once in update and remove", u.ItemID)
		}
		seen[u.ItemID] = true
		if u.Description == nil && u.PriceMinor == nil {
			return n, fmt.Errorf("update[%d] must change description or price_minor", i)
		}
		nu := normUpdate{ItemID: u.ItemID}
		if u.Description != nil {
			d, err := text(*u.Description, fmt.Sprintf("update[%d].description", i))
			if err != nil {
				return n, err
			}
			nu.Description = &d
		}
		if u.PriceMinor != nil {
			p, err := amount(u.PriceMinor, fmt.Sprintf("update[%d].price_minor", i), true)
			if err != nil {
				return n, err
			}
			nu.PriceMinor = &p
		}
		n.Update = append(n.Update, nu)
	}
	for _, id := range req.Remove {
		if id == uuid.Nil {
			return n, errors.New("remove contains an empty id")
		}
		if seen[id] {
			return n, fmt.Errorf("item %s appears more than once in update and remove", id)
		}
		seen[id] = true
		n.Remove = append(n.Remove, id)
	}
	for i, it := range req.Add {
		var ni normItem
		var err error
		if ni.Description, err = text(it.Description, fmt.Sprintf("add[%d].description", i)); err != nil {
			return n, err
		}
		if ni.PriceMinor, err = amount(it.PriceMinor, fmt.Sprintf("add[%d].price_minor", i), true); err != nil {
			return n, err
		}
		n.Add = append(n.Add, ni)
	}
	if req.TaxMinor != nil {
		v, e := amount(req.TaxMinor, "tax_minor", true)
		if e != nil {
			return n, e
		}
		n.TaxMinor = &v
	}
	if req.TipMinor != nil {
		v, e := amount(req.TipMinor, "tip_minor", true)
		if e != nil {
			return n, e
		}
		n.TipMinor = &v
	}
	slices.SortFunc(n.Update, func(a, b normUpdate) int { return cmpUUID(a.ItemID, b.ItemID) })
	slices.SortFunc(n.Remove, cmpUUID)
	return n, nil
}

// payerPrefix is the part shared by edit and confirm: path ids, then authorization
// WITHOUT a write lock (404 for a non-member, an unknown expense or another group's
// expense; 403 for a member who is not the payer, so nobody else can hold up
// claimers by calling this). It writes the response and returns false if the
// request must stop.
func (h *Handler) payerPrefix(w http.ResponseWriter, r *http.Request, action string) (user db.User, groupID, expenseID uuid.UUID, ok bool) {
	user, _ = auth.UserFrom(r.Context())
	groupID, err1 := uuid.Parse(r.PathValue("id"))
	expenseID, err2 := uuid.Parse(r.PathValue("expenseId"))
	if err1 != nil || err2 != nil {
		expenseNotFound(w)
		return user, groupID, expenseID, false
	}
	e, err := h.q.GetExpenseForMember(r.Context(), db.GetExpenseForMemberParams{UserID: user.ID, ExpenseID: expenseID, GroupID: groupID})
	if errors.Is(err, pgx.ErrNoRows) {
		expenseNotFound(w)
		return user, groupID, expenseID, false
	}
	if err != nil {
		h.internalError(w, "get expense", err)
		return user, groupID, expenseID, false
	}
	if e.PayerID != user.ID {
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "only the payer can "+action+" this expense")
		return user, groupID, expenseID, false
	}
	return user, groupID, expenseID, true
}

// edit changes the items, tax and tip of an expense that is being reviewed
// (`needs_review`) or claimed (`claiming`). Payer only.
//
// Order of checks: path ids and authorization without a write lock, the
// Idempotency-Key, the body (400s that do not claim the key), then one idempotent
// transaction that takes FOR NO KEY UPDATE on the expense row.
//
// That lock waits for every in-flight claim (they hold FOR SHARE) and blocks new
// ones until commit, so the claims deleted below are all there is. Under the lock
// the expense's version is compared with the one the client edited: a stale client
// gets 409 version_conflict instead of silently overwriting a newer edit. Claims do
// not change the version, so a claim made after the client read does not make an
// edit stale; it is deleted by the edit if it is on an item whose price changes.
//
// A claim says "I will pay my share of this item at this price", so claims on an
// item whose price changed, and on an item removed, are deleted in the same
// transaction. A description-only change keeps them. Tax and tip are not claimed
// (they are prorated at finalize), so changing them touches no claim.
func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
	user, groupID, expenseID, ok := h.payerPrefix(w, r, "edit")
	if !ok {
		return
	}
	key, err := idempotency.ParseKey(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req editRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	n, err := req.normalize()
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	canon, err := json.Marshal(n)
	if err != nil {
		h.internalError(w, "marshal normalized edit", err)
		return
	}
	fp := idempotency.Fingerprint("PATCH", "/v1/groups/{id}/expenses/{expenseId}", groupID.String(), expenseID.String(), string(canon))
	out, err := idempotency.Do(r.Context(), h.pool, user.ID, key, fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return h.doEdit(ctx, tx, user.ID, groupID, expenseID, n)
	})
	h.writeIdempotent(w, out, err, "edit expense")
}

func (h *Handler) writeIdempotent(w http.ResponseWriter, out idempotency.Outcome, err error, what string) {
	if errors.Is(err, idempotency.ErrKeyReuse) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "Idempotency-Key was already used with a different request")
		return
	}
	if err != nil {
		h.internalError(w, what, err)
		return
	}
	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	_, _ = w.Write(out.Body)
}

// doEdit runs inside the idempotency transaction. Every early return of a non-2xx
// result happens before anything is written, because that result is committed and
// replayed for the key.
func (h *Handler) doEdit(ctx context.Context, tx pgx.Tx, payerID, groupID, expenseID uuid.UUID, n normEdit) (idempotency.Result, error) {
	q := h.q.WithTx(tx)

	locked, err := q.LockExpenseForEdit(ctx, db.LockExpenseForEditParams{UserID: payerID, ExpenseID: expenseID, GroupID: groupID})
	if errors.Is(err, pgx.ErrNoRows) {
		return jsonResult(http.StatusNotFound, errBody("not_found", "expense not found")), nil
	}
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("lock expense: %w", err)
	}
	if locked.Status != statusClaiming && locked.Status != statusNeedsReview {
		return jsonResult(http.StatusConflict, errBody(codeInvalidState,
			"expense is "+locked.Status+"; only a needs_review or claiming expense can be edited")), nil
	}
	if locked.Version != n.Version {
		return jsonResult(http.StatusConflict, errBody(codeVersionConflict,
			fmt.Sprintf("expense is at version %d, not %d; read it again and redo the edit", locked.Version, n.Version))), nil
	}

	// The item set and the claim set are stable from here to commit.
	rows, err := q.ListExpenseItems(ctx, expenseID)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("list items: %w", err)
	}
	byID := make(map[uuid.UUID]db.ListExpenseItemsRow, len(rows))
	maxPos := int32(-1)
	for _, row := range rows {
		byID[row.ID] = row
		maxPos = max(maxPos, row.Position)
	}
	for _, u := range n.Update {
		if _, ok := byID[u.ItemID]; !ok {
			return jsonResult(http.StatusNotFound, errBody(codeItemNotFound, "item "+u.ItemID.String()+" is not in this expense")), nil
		}
	}
	for _, id := range n.Remove {
		if _, ok := byID[id]; !ok {
			return jsonResult(http.StatusNotFound, errBody(codeItemNotFound, "item "+id.String()+" is not in this expense")), nil
		}
	}

	// Work out the final state, so it can be checked before anything is written.
	type change struct {
		id          uuid.UUID
		description string
		price       int64
	}
	var changes []change
	dropClaims := slices.Clone(n.Remove)
	removed := make(map[uuid.UUID]bool, len(n.Remove))
	for _, id := range n.Remove {
		removed[id] = true
	}
	newPrice := make(map[uuid.UUID]int64, len(n.Update))
	for _, u := range n.Update {
		cur := byID[u.ItemID]
		c := change{id: u.ItemID, description: cur.Description, price: cur.PriceMinor}
		if u.Description != nil {
			c.description = *u.Description
		}
		if u.PriceMinor != nil {
			c.price = *u.PriceMinor
		}
		if c.description == cur.Description && c.price == cur.PriceMinor {
			continue // nothing actually changes for this item
		}
		if c.price != cur.PriceMinor {
			dropClaims = append(dropClaims, c.id)
		}
		changes = append(changes, c)
		newPrice[c.id] = c.price
	}
	finalCount := len(n.Add)
	var subtotal int64
	for _, row := range rows {
		if removed[row.ID] {
			continue
		}
		finalCount++
		if p, ok := newPrice[row.ID]; ok {
			subtotal += p
		} else {
			subtotal += row.PriceMinor
		}
	}
	for _, a := range n.Add {
		subtotal += a.PriceMinor
	}
	tax, tip := locked.TaxMinor, locked.TipMinor
	if n.TaxMinor != nil {
		tax = *n.TaxMinor
	}
	if n.TipMinor != nil {
		tip = *n.TipMinor
	}
	if finalCount > maxItems {
		return jsonResult(http.StatusBadRequest, errBody("invalid_request", fmt.Sprintf("an expense has at most %d items", maxItems))), nil
	}
	if locked.Status == statusClaiming {
		// A claiming expense must stay finalizable. (A needs_review one is a draft;
		// confirm checks the same things before it opens for claiming.)
		if msg := finalizableProblem(finalCount, subtotal, tax, tip); msg != "" {
			return jsonResult(http.StatusBadRequest, errBody("invalid_request", msg)), nil
		}
	}

	claimsRemoved := int64(0)
	if len(dropClaims) > 0 {
		if claimsRemoved, err = q.DeleteClaimsForItems(ctx, db.DeleteClaimsForItemsParams{ExpenseID: expenseID, ItemIds: dropClaims}); err != nil {
			return idempotency.Result{}, fmt.Errorf("delete claims: %w", err)
		}
	}
	if len(n.Remove) > 0 {
		if got, err := q.DeleteExpenseItems(ctx, db.DeleteExpenseItemsParams{ExpenseID: expenseID, ItemIds: n.Remove}); err != nil || got != int64(len(n.Remove)) {
			return idempotency.Result{}, fmt.Errorf("delete items: rows=%d err=%v", got, err)
		}
	}
	for _, c := range changes {
		if got, err := q.UpdateExpenseItem(ctx, db.UpdateExpenseItemParams{ID: c.id, ExpenseID: expenseID, Description: c.description, PriceMinor: c.price}); err != nil || got != 1 {
			return idempotency.Result{}, fmt.Errorf("update item: rows=%d err=%v", got, err)
		}
	}
	if len(n.Add) > 0 {
		p := db.CreateExpenseItemsParams{ExpenseID: expenseID}
		for i, a := range n.Add {
			p.Positions = append(p.Positions, maxPos+1+int32(i))
			p.Descriptions = append(p.Descriptions, a.Description)
			p.Prices = append(p.Prices, a.PriceMinor)
		}
		if _, err := q.CreateExpenseItems(ctx, p); err != nil {
			return idempotency.Result{}, fmt.Errorf("insert items: %w", err)
		}
	}
	// Last, like MarkExpenseFinalized: conditional on the version read under the lock.
	if got, err := q.MarkExpenseEdited(ctx, db.MarkExpenseEditedParams{ID: expenseID, TaxMinor: tax, TipMinor: tip, Version: locked.Version}); err != nil || got != 1 {
		return idempotency.Result{}, fmt.Errorf("mark edited: rows=%d err=%v", got, err)
	}

	if err := h.audit(ctx, q, payerID, groupID, "expense.items_edited", map[string]any{
		"expense_id": expenseID, "version": locked.Version + 1,
		"added": len(n.Add), "updated": len(changes), "removed": len(n.Remove), "claims_removed": claimsRemoved,
	}); err != nil {
		return idempotency.Result{}, fmt.Errorf("audit: %w", err)
	}
	return h.expenseResult(ctx, q, http.StatusOK, payerID, groupID, expenseID)
}

// finalizableProblem says why an expense with these items could never be finalized
// (or "" if nothing is wrong). The same rules as create.
func finalizableProblem(itemCount int, subtotal, tax, tip int64) string {
	if itemCount < 1 {
		return "an expense needs at least one item"
	}
	if subtotal == 0 && (tax > 0 || tip > 0) {
		return "tax_minor and tip_minor must be 0 when every item is free"
	}
	return ""
}

// expenseResult re-reads the expense inside the transaction and renders it.
func (h *Handler) expenseResult(ctx context.Context, q *db.Queries, status int, userID, groupID, expenseID uuid.UUID) (idempotency.Result, error) {
	view, err := q.GetExpenseForMember(ctx, db.GetExpenseForMemberParams{UserID: userID, ExpenseID: expenseID, GroupID: groupID})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("re-read expense: %w", err)
	}
	rows, err := q.ListExpenseItems(ctx, expenseID)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("re-list items: %w", err)
	}
	return jsonResult(status, expenseView(view, rows)), nil
}

// ---- confirm ----

type confirmRequest struct {
	Version *int32 `json:"version"`
}

// confirm ends a review: the payer says the items match the receipt, and the
// expense opens for claiming. Payer only, needs_review only, and only at the
// version the payer reviewed (an edit in another tab makes it stale).
func (h *Handler) confirm(w http.ResponseWriter, r *http.Request) {
	user, groupID, expenseID, ok := h.payerPrefix(w, r, "confirm")
	if !ok {
		return
	}
	key, err := idempotency.ParseKey(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req confirmRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Version == nil || *req.Version < 1 {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "version is required (the version of the expense you reviewed)")
		return
	}
	version := *req.Version
	fp := idempotency.Fingerprint("POST", "/v1/groups/{id}/expenses/{expenseId}/confirm", groupID.String(), expenseID.String(), fmt.Sprint(version))
	out, err := idempotency.Do(r.Context(), h.pool, user.ID, key, fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return h.doConfirm(ctx, tx, user.ID, groupID, expenseID, version)
	})
	h.writeIdempotent(w, out, err, "confirm expense")
}

func (h *Handler) doConfirm(ctx context.Context, tx pgx.Tx, payerID, groupID, expenseID uuid.UUID, version int32) (idempotency.Result, error) {
	q := h.q.WithTx(tx)
	locked, err := q.LockExpenseForEdit(ctx, db.LockExpenseForEditParams{UserID: payerID, ExpenseID: expenseID, GroupID: groupID})
	if errors.Is(err, pgx.ErrNoRows) {
		return jsonResult(http.StatusNotFound, errBody("not_found", "expense not found")), nil
	}
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("lock expense: %w", err)
	}
	if locked.Status != statusNeedsReview {
		return jsonResult(http.StatusConflict, errBody(codeInvalidState,
			"expense is "+locked.Status+"; only a needs_review expense can be confirmed")), nil
	}
	if locked.Version != version {
		return jsonResult(http.StatusConflict, errBody(codeVersionConflict,
			fmt.Sprintf("expense is at version %d, not %d; read it again and review it", locked.Version, version))), nil
	}
	rows, err := q.ListExpenseItems(ctx, expenseID)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("list items: %w", err)
	}
	var subtotal int64
	for _, row := range rows {
		subtotal += row.PriceMinor
	}
	if msg := finalizableProblem(len(rows), subtotal, locked.TaxMinor, locked.TipMinor); msg != "" {
		return jsonResult(http.StatusBadRequest, errBody("invalid_request", msg)), nil
	}
	if got, err := q.MarkExpenseConfirmed(ctx, db.MarkExpenseConfirmedParams{ID: expenseID, Version: locked.Version}); err != nil || got != 1 {
		return idempotency.Result{}, fmt.Errorf("mark confirmed: rows=%d err=%v", got, err)
	}
	if err := h.audit(ctx, q, payerID, groupID, "expense.confirmed", map[string]any{
		"expense_id": expenseID, "version": locked.Version + 1, "items": len(rows),
	}); err != nil {
		return idempotency.Result{}, fmt.Errorf("audit: %w", err)
	}
	return h.expenseResult(ctx, q, http.StatusOK, payerID, groupID, expenseID)
}
