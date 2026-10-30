// Package expenses implements creating and reading expenses within a group.
//
// Claiming, finalizing, voiding and item edits are added in later M2 steps.
// Every method assumes auth.Middleware has already run.
package expenses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/httpx"
	"github.com/riyne/fifty-fifty/internal/idempotency"
)

// Input limits. They bound request cost and keep every sum far from int64
// overflow (100 items of 100,000,000 is 1e10). Amounts are minor units, so
// 100,000,000 is 1,000,000.00 in either currency.
const (
	maxItems       = 100
	maxAmountMinor = 100_000_000
	maxTextRunes   = 200
)

type Handler struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool, q: db.New(pool)}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/groups/{id}/expenses", h.create)
	mux.HandleFunc("GET /v1/groups/{id}/expenses", h.list)
	mux.HandleFunc("GET /v1/groups/{id}/expenses/{expenseId}", h.get)
	mux.HandleFunc("POST /v1/groups/{id}/expenses/{expenseId}/finalize", h.finalize)
	mux.HandleFunc("PUT /v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", h.claim)
	mux.HandleFunc("DELETE /v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", h.unclaim)
}

type itemJSON struct {
	ID          uuid.UUID   `json:"id"`
	Position    int32       `json:"position"`
	Description string      `json:"description"`
	PriceMinor  int64       `json:"price_minor"`
	ClaimedBy   []uuid.UUID `json:"claimed_by"` // member ids, sorted
}

type expenseJSON struct {
	ID            uuid.UUID  `json:"id"`
	GroupID       uuid.UUID  `json:"group_id"`
	PayerID       uuid.UUID  `json:"payer_id"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	Version       int32      `json:"version"`
	Currency      string     `json:"currency"`
	TaxMinor      int64      `json:"tax_minor"`
	TipMinor      int64      `json:"tip_minor"`
	SubtotalMinor int64      `json:"subtotal_minor"` // sum of item prices
	TotalMinor    int64      `json:"total_minor"`    // subtotal + tax + tip; derived, never stored
	CreatedAt     time.Time  `json:"created_at"`
	FinalizedAt   *time.Time `json:"finalized_at"`
	VoidedAt      *time.Time `json:"voided_at"`
	Items         []itemJSON `json:"items"`
}

type summaryJSON struct {
	ID            uuid.UUID `json:"id"`
	PayerID       uuid.UUID `json:"payer_id"`
	Description   string    `json:"description"`
	Status        string    `json:"status"`
	Currency      string    `json:"currency"`
	ItemCount     int32     `json:"item_count"`
	SubtotalMinor int64     `json:"subtotal_minor"`
	TotalMinor    int64     `json:"total_minor"`
	CreatedAt     time.Time `json:"created_at"`
}

// ---- create ----

type itemRequest struct {
	Description string `json:"description"`
	PriceMinor  *int64 `json:"price_minor"` // pointer: a missing price is an error, not 0
}

type createRequest struct {
	Description string        `json:"description"`
	Items       []itemRequest `json:"items"`
	TaxMinor    *int64        `json:"tax_minor"`
	TipMinor    *int64        `json:"tip_minor"`
}

// normalized is a validated request with defaults applied and text trimmed. It
// is also what gets fingerprinted, so two requests that mean the same thing
// (different whitespace, key order, omitted zero tax) count as the same request.
type normalized struct {
	Description string     `json:"description"`
	Items       []normItem `json:"items"`
	TaxMinor    int64      `json:"tax_minor"`
	TipMinor    int64      `json:"tip_minor"`
}

type normItem struct {
	Description string `json:"description"`
	PriceMinor  int64  `json:"price_minor"`
}

func (n normalized) subtotal() (sum int64) {
	for _, it := range n.Items {
		sum += it.PriceMinor // bounded by maxItems * maxAmountMinor
	}
	return sum
}

func text(s, field string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n < 1 || n > maxTextRunes {
		return "", fmt.Errorf("%s must be 1-%d characters", field, maxTextRunes)
	}
	return s, nil
}

func amount(v *int64, field string, required bool) (int64, error) {
	if v == nil {
		if required {
			return 0, fmt.Errorf("%s is required", field)
		}
		return 0, nil
	}
	if *v < 0 || *v > maxAmountMinor {
		return 0, fmt.Errorf("%s must be between 0 and %d (minor units)", field, maxAmountMinor)
	}
	return *v, nil
}

func (req createRequest) normalize() (normalized, error) {
	var n normalized
	var err error
	if n.Description, err = text(req.Description, "description"); err != nil {
		return n, err
	}
	if len(req.Items) < 1 || len(req.Items) > maxItems {
		return n, fmt.Errorf("items must contain 1-%d entries", maxItems)
	}
	n.Items = make([]normItem, len(req.Items))
	for i, it := range req.Items {
		if n.Items[i].Description, err = text(it.Description, fmt.Sprintf("items[%d].description", i)); err != nil {
			return n, err
		}
		if n.Items[i].PriceMinor, err = amount(it.PriceMinor, fmt.Sprintf("items[%d].price_minor", i), true); err != nil {
			return n, err
		}
	}
	if n.TaxMinor, err = amount(req.TaxMinor, "tax_minor", false); err != nil {
		return n, err
	}
	if n.TipMinor, err = amount(req.TipMinor, "tip_minor", false); err != nil {
		return n, err
	}
	// Tax and tip are prorated by claimed subtotal; with nothing priced above zero
	// there is nobody to charge them to, and finalize could never succeed.
	if n.subtotal() == 0 && (n.TaxMinor > 0 || n.TipMinor > 0) {
		return n, errors.New("tax_minor and tip_minor must be 0 when every item is free")
	}
	return n, nil
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	// Membership first, before looking at the key or the body: a non-member
	// learns nothing, not even whether their request was well formed.
	groupID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		groupNotFound(w)
		return
	}
	g, err := h.q.GetGroupForMember(r.Context(), db.GetGroupForMemberParams{ID: groupID, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		groupNotFound(w)
		return
	}
	if err != nil {
		h.internalError(w, "get group", err)
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
	n, err := req.normalize()
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	canon, err := json.Marshal(n)
	if err != nil {
		h.internalError(w, "marshal normalized request", err)
		return
	}
	fp := idempotency.Fingerprint("POST", "/v1/groups/{id}/expenses", groupID.String(), string(canon))
	out, err := idempotency.Do(r.Context(), h.pool, user.ID, key, fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return h.insert(ctx, tx, user.ID, g.ID, g.Currency, n)
	})
	if errors.Is(err, idempotency.ErrKeyReuse) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "Idempotency-Key was already used with a different request")
		return
	}
	if err != nil {
		h.internalError(w, "create expense", err)
		return
	}
	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	_, _ = w.Write(out.Body)
}

// insert runs inside the idempotency transaction. The expense and its items
// commit together or not at all. The creator is the payer (DESIGN.md, M2
// decisions); the foreign key on (group_id, payer_id) makes a non-member payer
// impossible even if the caller's membership check were removed.
func (h *Handler) insert(ctx context.Context, tx pgx.Tx, userID, groupID uuid.UUID, currency string, n normalized) (idempotency.Result, error) {
	q := h.q.WithTx(tx)
	e, err := q.CreateExpense(ctx, db.CreateExpenseParams{
		GroupID: groupID, PayerID: userID, Description: n.Description, TaxMinor: n.TaxMinor, TipMinor: n.TipMinor,
	})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("insert expense: %w", err)
	}

	p := db.CreateExpenseItemsParams{ExpenseID: e.ID}
	for i, it := range n.Items {
		p.Positions = append(p.Positions, int32(i))
		p.Descriptions = append(p.Descriptions, it.Description)
		p.Prices = append(p.Prices, it.PriceMinor)
	}
	rows, err := q.CreateExpenseItems(ctx, p)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("insert items: %w", err)
	}
	items := make([]itemJSON, len(rows))
	for _, row := range rows { // RETURNING order is not guaranteed; place by position
		items[row.Position] = itemJSON{ID: row.ID, Position: row.Position, Description: row.Description, PriceMinor: row.PriceMinor, ClaimedBy: []uuid.UUID{}}
	}

	sub := n.subtotal()
	return jsonResult(http.StatusCreated, expenseJSON{
		ID: e.ID, GroupID: e.GroupID, PayerID: e.PayerID, Description: e.Description, Status: e.Status, Version: e.Version,
		Currency: currency, TaxMinor: e.TaxMinor, TipMinor: e.TipMinor,
		SubtotalMinor: sub, TotalMinor: sub + e.TaxMinor + e.TipMinor,
		CreatedAt: e.CreatedAt, FinalizedAt: e.FinalizedAt, VoidedAt: e.VoidedAt, Items: items,
	}), nil
}

// ---- read ----

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	groupID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		groupNotFound(w)
		return
	}
	// An empty list for a member and a 404 for a non-member must be
	// distinguishable, so check membership explicitly.
	if _, err := h.q.GetGroupForMember(r.Context(), db.GetGroupForMemberParams{ID: groupID, UserID: user.ID}); errors.Is(err, pgx.ErrNoRows) {
		groupNotFound(w)
		return
	} else if err != nil {
		h.internalError(w, "get group", err)
		return
	}
	rows, err := h.q.ListExpensesForMember(r.Context(), db.ListExpensesForMemberParams{UserID: user.ID, GroupID: groupID})
	if err != nil {
		h.internalError(w, "list expenses", err)
		return
	}
	out := make([]summaryJSON, 0, len(rows))
	for _, e := range rows {
		out = append(out, summaryJSON{
			ID: e.ID, PayerID: e.PayerID, Description: e.Description, Status: e.Status, Currency: e.Currency,
			ItemCount: e.ItemCount, SubtotalMinor: e.SubtotalMinor, TotalMinor: e.SubtotalMinor + e.TaxMinor + e.TipMinor,
			CreatedAt: e.CreatedAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"expenses": out})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	// A malformed id, an unknown id, an expense in another group, and a group the
	// caller is not in all return the same 404 from the same single query.
	groupID, err1 := uuid.Parse(r.PathValue("id"))
	expenseID, err2 := uuid.Parse(r.PathValue("expenseId"))
	if err1 != nil || err2 != nil {
		expenseNotFound(w)
		return
	}

	// One read-only snapshot, so the expense and its items are consistent with
	// each other even if a claim or edit commits between the two queries.
	tx, err := h.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		h.internalError(w, "begin read tx", err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // read-only; nothing to commit
	q := h.q.WithTx(tx)

	e, err := q.GetExpenseForMember(r.Context(), db.GetExpenseForMemberParams{UserID: user.ID, ExpenseID: expenseID, GroupID: groupID})
	if errors.Is(err, pgx.ErrNoRows) {
		expenseNotFound(w)
		return
	}
	if err != nil {
		h.internalError(w, "get expense", err)
		return
	}
	rows, err := q.ListExpenseItems(r.Context(), e.ID)
	if err != nil {
		h.internalError(w, "list expense items", err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, expenseView(e, rows))
}

// expenseView renders an expense and its items. It is shared by the read
// endpoint and by finalize, so both show exactly the same thing.
func expenseView(e db.GetExpenseForMemberRow, rows []db.ListExpenseItemsRow) expenseJSON {
	items := make([]itemJSON, len(rows))
	for i, row := range rows {
		items[i] = itemJSON{ID: row.ID, Position: row.Position, Description: row.Description, PriceMinor: row.PriceMinor, ClaimedBy: row.ClaimedBy}
	}
	return expenseJSON{
		ID: e.ID, GroupID: e.GroupID, PayerID: e.PayerID, Description: e.Description, Status: e.Status, Version: e.Version,
		Currency: e.Currency, TaxMinor: e.TaxMinor, TipMinor: e.TipMinor,
		SubtotalMinor: e.SubtotalMinor, TotalMinor: e.SubtotalMinor + e.TaxMinor + e.TipMinor,
		CreatedAt: e.CreatedAt, FinalizedAt: e.FinalizedAt, VoidedAt: e.VoidedAt, Items: items,
	}
}

// ---- helpers ----

func groupNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "group not found")
}

func expenseNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "expense not found")
}

func (h *Handler) internalError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
}

func jsonResult(status int, v any) idempotency.Result {
	b, err := json.Marshal(v)
	if err != nil {
		// Only reachable with an unmarshalable type, i.e. a programming error.
		panic("expenses: marshal response: " + err.Error())
	}
	return idempotency.Result{Status: status, Body: append(b, '\n')}
}
