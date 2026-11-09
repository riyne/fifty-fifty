package expenses

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/controls"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/httpx"
)

// statusClaiming is the only expense status in which claims may change.
const statusClaiming = "claiming"

func (h *Handler) claim(w http.ResponseWriter, r *http.Request)   { h.setClaim(w, r, true) }
func (h *Handler) unclaim(w http.ResponseWriter, r *http.Request) { h.setClaim(w, r, false) }

// setClaim adds or removes the caller's own claim on one item.
//
// Locking (docs/ARCHITECTURE.md section 8.2): the first statement takes FOR SHARE on the
// expense row. Claimers share that lock, so they never block each other, but
// finalize/void/item edits take FOR UPDATE on the same row and so wait for every
// in-flight claim, then read a stable claim set. A claim that arrives while
// finalize holds the lock waits, then re-reads the row, sees `finalized`, and is
// rejected with 409 below. The status check and the write happen under the same
// lock, which is what stops a claim from slipping in after finalize has read the
// claim set.
//
// Both operations are idempotent by nature (claiming twice or unclaiming a
// claim you do not hold changes nothing), so they take no Idempotency-Key.
func (h *Handler) setClaim(w http.ResponseWriter, r *http.Request, claim bool) {
	user, _ := auth.UserFrom(r.Context())

	groupID, err1 := uuid.Parse(r.PathValue("id"))
	expenseID, err2 := uuid.Parse(r.PathValue("expenseId"))
	if err1 != nil || err2 != nil {
		expenseNotFound(w)
		return
	}
	// A malformed item id is treated as an id that matches nothing, but only after
	// the membership check, so a non-member sees "expense not found" either way.
	itemID, err := uuid.Parse(r.PathValue("itemId"))
	if err != nil {
		itemID = uuid.Nil
	}

	tx, err := h.scope.Begin(r.Context(), user.ID)
	if err != nil {
		h.internalError(w, "begin tx", err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // no-op after Commit
	q := db.New(tx)

	status, err := controls.LockExpenseForClaim(r.Context(), q, db.LockExpenseForClaimParams{UserID: user.ID, ExpenseID: expenseID, GroupID: groupID})
	if errors.Is(err, pgx.ErrNoRows) {
		expenseNotFound(w)
		return
	}
	if err != nil {
		h.internalError(w, "lock expense for claim", err)
		return
	}
	if status != statusClaiming {
		httpx.WriteError(w, http.StatusConflict, "expense_not_claimable", "expense is "+status+"; claims can only change while it is claiming")
		return
	}

	item, err := q.GetExpenseItem(r.Context(), db.GetExpenseItemParams{ID: itemID, ExpenseID: expenseID})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "item not found")
		return
	}
	if err != nil {
		h.internalError(w, "get item", err)
		return
	}

	if claim {
		if _, err := q.AddClaim(r.Context(), db.AddClaimParams{ItemID: itemID, ExpenseID: expenseID, GroupID: groupID, MemberID: user.ID}); err != nil {
			h.internalError(w, "add claim", err)
			return
		}
		// Re-read inside the transaction so the response includes this claim.
		item, err = q.GetExpenseItem(r.Context(), db.GetExpenseItemParams{ID: itemID, ExpenseID: expenseID})
		if err != nil {
			h.internalError(w, "re-read item", err)
			return
		}
	} else if _, err := q.RemoveClaim(r.Context(), db.RemoveClaimParams{ItemID: itemID, ExpenseID: expenseID, MemberID: user.ID}); err != nil {
		h.internalError(w, "remove claim", err)
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		h.internalError(w, "commit", err)
		return
	}
	if !claim {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, itemJSON{
		ID: item.ID, Position: item.Position, Description: item.Description, PriceMinor: item.PriceMinor, ClaimedBy: item.ClaimedBy,
	})
}
