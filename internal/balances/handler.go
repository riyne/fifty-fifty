// Package balances serves a group's net balances and suggested settle-up transfers.
// Both are derived at read time from the ledger; nothing here writes.
package balances

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/dbscope"
	"github.com/riyne/fifty-fifty/internal/httpx"
	"github.com/riyne/fifty-fifty/internal/ledger"
)

type Handler struct {
	scope *dbscope.DB
}

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{scope: dbscope.New(pool)} }

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/groups/{id}/balances", h.get)
}

type balanceJSON struct {
	MemberID     uuid.UUID `json:"member_id"`
	DisplayName  string    `json:"display_name"`
	BalanceMinor int64     `json:"balance_minor"` // positive: the group owes them; negative: they owe the group
}

type transferJSON struct {
	FromMemberID uuid.UUID `json:"from_member_id"` // pays
	ToMemberID   uuid.UUID `json:"to_member_id"`   // receives
	AmountMinor  int64     `json:"amount_minor"`
}

type response struct {
	Currency           string         `json:"currency"`
	Balances           []balanceJSON  `json:"balances"`
	SuggestedTransfers []transferJSON `json:"suggested_transfers"`
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	// A malformed id, an unknown group and a group the caller is not in all give
	// the same 404 as every other group-scoped read.
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

	rows, err := h.scope.Reader(user.ID).GetGroupBalances(r.Context(), db.GetGroupBalancesParams{CallerID: user.ID, GroupID: groupID})
	if err != nil {
		internalError(w, "get balances", err)
		return
	}
	out := response{Currency: g.Currency, Balances: make([]balanceJSON, len(rows)), SuggestedTransfers: []transferJSON{}}
	in := make([]ledger.Balance, len(rows))
	for i, row := range rows {
		out.Balances[i] = balanceJSON{MemberID: row.MemberID, DisplayName: row.DisplayName, BalanceMinor: row.BalanceMinor}
		in[i] = ledger.Balance{MemberID: row.MemberID, Amount: row.BalanceMinor}
	}
	transfers, err := ledger.SuggestTransfers(in)
	if err != nil {
		// The database enforces that every posting sums to zero, so this means the
		// ledger itself is inconsistent. Fail loudly rather than show wrong money.
		internalError(w, "suggest transfers (ledger does not balance?)", err)
		return
	}
	for _, t := range transfers {
		out.SuggestedTransfers = append(out.SuggestedTransfers, transferJSON{FromMemberID: t.From, ToMemberID: t.To, AmountMinor: t.Amount})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func groupNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "group not found")
}

func internalError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
}
