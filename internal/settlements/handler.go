package settlements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/controls"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/dbscope"
	"github.com/riyne/fifty-fifty/internal/httpx"
	"github.com/riyne/fifty-fifty/internal/idempotency"
	"github.com/riyne/fifty-fifty/internal/queue"
)

// KindExecute is the job kind that sends a settlement to the payment provider.
const KindExecute = "settlement.execute"

// ExecutePayload is the payload of a KindExecute job: the id and nothing else, so
// the handler always reads the settlement's current state.
type ExecutePayload struct {
	SettlementID uuid.UUID `json:"settlement_id"`
}

// maxAmountMinor is the same bound as an expense amount (1,000,000.00).
const maxAmountMinor = 100_000_000

const codeExceedsAvailable = "exceeds_available"

// Handler serves the settlement endpoints. It assumes auth.Middleware has run.
type Handler struct {
	scope *dbscope.DB
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{scope: dbscope.New(pool)}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/groups/{id}/settlements", h.create)
	mux.HandleFunc("GET /v1/groups/{id}/settlements", h.list)
	mux.HandleFunc("GET /v1/groups/{id}/settlements/{settlementId}", h.get)
}

type settlementJSON struct {
	ID                  uuid.UUID  `json:"id"`
	GroupID             uuid.UUID  `json:"group_id"`
	FromMemberID        uuid.UUID  `json:"from_member_id"` // the sender, who created it
	ToMemberID          uuid.UUID  `json:"to_member_id"`
	AmountMinor         int64      `json:"amount_minor"`
	Currency            string     `json:"currency"`
	Status              Status     `json:"status"`
	FailureCode         *string    `json:"failure_code"`
	LedgerTransactionID *uuid.UUID `json:"ledger_transaction_id"`
	CreatedAt           time.Time  `json:"created_at"`
	ResolvedAt          *time.Time `json:"resolved_at"`
}

func render(s db.Settlement, currency string) settlementJSON {
	out := settlementJSON{
		ID: s.ID, GroupID: s.GroupID, FromMemberID: s.FromMember, ToMemberID: s.ToMember,
		AmountMinor: s.AmountMinor, Currency: currency, Status: Status(s.Status),
		CreatedAt: s.CreatedAt, ResolvedAt: s.ResolvedAt,
	}
	if s.FailureCode.Valid {
		out.FailureCode = &s.FailureCode.String
	}
	if s.LedgerTransactionID.Valid {
		id := uuid.UUID(s.LedgerTransactionID.Bytes)
		out.LedgerTransactionID = &id
	}
	return out
}

// Available is how much the sender may still send to the payee. The ledger holds
// net balances, not pairwise debts, so the rule is on the two members' group
// balances: the sender can send at most what they owe the group, and the payee can
// receive at most what the group owes them, each less what their open settlements
// have already reserved. Never negative.
func Available(fromBalance, toBalance, reservedOut, reservedIn int64) int64 {
	canSend := -fromBalance - reservedOut
	canReceive := toBalance - reservedIn
	return max(0, min(canSend, canReceive))
}

// ---- create ----

type createRequest struct {
	ToMemberID  *uuid.UUID `json:"to_member_id"`
	AmountMinor *int64     `json:"amount_minor"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	// Membership first, before the key or the body, as for expenses: a non-member
	// learns nothing, not even whether their request was well formed.
	groupID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		groupNotFound(w)
		return
	}
	g, err := h.scope.Reader(user.ID).GetGroupForMember(r.Context(), db.GetGroupForMemberParams{ID: groupID, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		groupNotFound(w)
		return
	}
	if err != nil {
		internalError(w, "get group", err)
		return
	}

	key, err := idempotency.ParseKey(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	switch {
	case req.ToMemberID == nil:
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "to_member_id is required")
		return
	case req.AmountMinor == nil:
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "amount_minor is required")
		return
	case *req.AmountMinor < 1 || *req.AmountMinor > maxAmountMinor:
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("amount_minor must be between 1 and %d (minor units)", maxAmountMinor))
		return
	case *req.ToMemberID == user.ID:
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "to_member_id must be someone other than you")
		return
	}
	to, amount := *req.ToMemberID, *req.AmountMinor
	// The caller is a member, so they can already list the members: naming a
	// non-member reveals nothing. The foreign key on settlements is the backstop.
	isMember, err := h.scope.Reader(user.ID).IsGroupMember(r.Context(), db.IsGroupMemberParams{GroupID: groupID, UserID: to})
	if err != nil {
		internalError(w, "check payee membership", err)
		return
	}
	if !isMember {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "to_member_id is not a member of this group")
		return
	}

	fp := idempotency.Fingerprint("POST", "/v1/groups/{id}/settlements", groupID.String(), to.String(), strconv.FormatInt(amount, 10))
	out, err := idempotency.Do(r.Context(), h.scope, user.ID, key, fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return h.insert(ctx, tx, user.ID, to, groupID, g.Currency, amount)
	})
	if errors.Is(err, idempotency.ErrKeyReuse) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "Idempotency-Key was already used with a different request")
		return
	}
	if err != nil {
		internalError(w, "create settlement", err)
		return
	}
	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	_, _ = w.Write(out.Body)
}

// insert runs inside the idempotency transaction. The settlement, its execute
// job and the audit event commit together or not at all (no dual write). Every
// non-2xx result is returned before anything is written, because that result is
// committed and replayed for the key.
//
// The group row is locked first (FOR NO KEY UPDATE), so two creations in one group
// run one after the other: the second one's capacity read sees the first one's
// reservation. Without the lock both would read the same balances, both pass, and
// together reserve more than the sender owes.
func (h *Handler) insert(ctx context.Context, tx pgx.Tx, from, to, groupID uuid.UUID, currency string, amount int64) (idempotency.Result, error) {
	q := db.New(tx)

	if _, err := controls.LockGroupForSettlement(ctx, q, groupID); errors.Is(err, pgx.ErrNoRows) {
		return jsonResult(http.StatusNotFound, errBody("not_found", "group not found")), nil
	} else if err != nil {
		return idempotency.Result{}, fmt.Errorf("lock group: %w", err)
	}
	c, err := q.GetSettlementCapacity(ctx, db.GetSettlementCapacityParams{GroupID: groupID, FromMember: from, ToMember: to})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("read capacity: %w", err)
	}
	if avail := Available(c.FromBalance, c.ToBalance, c.ReservedOut, c.ReservedIn); amount > avail {
		return jsonResult(http.StatusConflict, errBody(codeExceedsAvailable,
			fmt.Sprintf("at most %d (minor units) can be settled from you to this member right now", avail))), nil
	}

	s, err := q.CreateSettlement(ctx, db.CreateSettlementParams{GroupID: groupID, FromMember: from, ToMember: to, AmountMinor: amount})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("insert settlement: %w", err)
	}
	if _, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: KindExecute, Payload: ExecutePayload{SettlementID: s.ID}}); err != nil {
		return idempotency.Result{}, fmt.Errorf("enqueue execute: %w", err)
	}
	data, err := json.Marshal(map[string]any{"settlement_id": s.ID, "to_member_id": to, "amount_minor": amount})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("audit data: %w", err)
	}
	if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{ActorUserID: from, GroupID: groupID, Kind: "settlement.created", Data: data}); err != nil {
		return idempotency.Result{}, fmt.Errorf("audit: %w", err)
	}
	return jsonResult(http.StatusAccepted, render(s, currency)), nil
}

// ---- read ----

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	groupID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		groupNotFound(w)
		return
	}
	// An empty list for a member and a 404 for a non-member must be distinguishable.
	if _, err := h.scope.Reader(user.ID).GetGroupForMember(r.Context(), db.GetGroupForMemberParams{ID: groupID, UserID: user.ID}); errors.Is(err, pgx.ErrNoRows) {
		groupNotFound(w)
		return
	} else if err != nil {
		internalError(w, "get group", err)
		return
	}
	rows, err := h.scope.Reader(user.ID).ListSettlementsForMember(r.Context(), db.ListSettlementsForMemberParams{UserID: user.ID, GroupID: groupID})
	if err != nil {
		internalError(w, "list settlements", err)
		return
	}
	out := make([]settlementJSON, len(rows))
	for i, s := range rows {
		out[i] = render(db.Settlement{
			ID: s.ID, GroupID: s.GroupID, FromMember: s.FromMember, ToMember: s.ToMember, AmountMinor: s.AmountMinor,
			Status: s.Status, FailureCode: s.FailureCode, LedgerTransactionID: s.LedgerTransactionID,
			CreatedAt: s.CreatedAt, ResolvedAt: s.ResolvedAt,
		}, s.Currency)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"settlements": out})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	// A malformed id, an unknown id, a settlement in another group, and a group the
	// caller is not in all return the same 404 from the same single query.
	groupID, err1 := uuid.Parse(r.PathValue("id"))
	settlementID, err2 := uuid.Parse(r.PathValue("settlementId"))
	if err1 != nil || err2 != nil {
		settlementNotFound(w)
		return
	}
	s, err := h.scope.Reader(user.ID).GetSettlementForMember(r.Context(), db.GetSettlementForMemberParams{UserID: user.ID, SettlementID: settlementID, GroupID: groupID})
	if errors.Is(err, pgx.ErrNoRows) {
		settlementNotFound(w)
		return
	}
	if err != nil {
		internalError(w, "get settlement", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, render(db.Settlement{
		ID: s.ID, GroupID: s.GroupID, FromMember: s.FromMember, ToMember: s.ToMember, AmountMinor: s.AmountMinor,
		Status: s.Status, FailureCode: s.FailureCode, LedgerTransactionID: s.LedgerTransactionID,
		CreatedAt: s.CreatedAt, ResolvedAt: s.ResolvedAt,
	}, s.Currency))
}

// ---- helpers ----

func groupNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "group not found")
}

func settlementNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "settlement not found")
}

func internalError(w http.ResponseWriter, what string, err error) {
	httpx.InternalError(w, what, err)
}

func errBody(code, message string) map[string]string {
	return map[string]string{"code": code, "message": message}
}

func jsonResult(status int, v any) idempotency.Result {
	b, err := json.Marshal(v)
	if err != nil {
		panic("settlements: marshal response: " + err.Error()) // an unmarshalable type: a programming error
	}
	return idempotency.Result{Status: status, Body: append(b, '\n')}
}
