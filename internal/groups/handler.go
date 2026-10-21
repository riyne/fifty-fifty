// Package groups implements group creation and membership-scoped reads.
package groups

import (
	"errors"
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
	"github.com/riyne/fifty-fifty/internal/ratelimit"
)

// Handler serves the /v1/groups endpoints. Every method assumes auth.Middleware
// has already run.
type Handler struct {
	pool        *pgxpool.Pool
	q           *db.Queries
	joinLimiter *ratelimit.Limiter
}

// NewHandler builds the groups handler. joinLimiter throttles POST /v1/join per
// user, which is what makes guessing join codes impractical.
func NewHandler(pool *pgxpool.Pool, joinLimiter *ratelimit.Limiter) *Handler {
	return &Handler{pool: pool, q: db.New(pool), joinLimiter: joinLimiter}
}

// Register adds the group routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/groups", h.create)
	mux.HandleFunc("GET /v1/groups", h.list)
	mux.HandleFunc("GET /v1/groups/{id}", h.get)

	mux.HandleFunc("POST /v1/groups/{id}/join-codes", h.createJoinCode)
	mux.HandleFunc("GET /v1/groups/{id}/join-codes", h.listJoinCodes)
	mux.HandleFunc("DELETE /v1/groups/{id}/join-codes/{codeId}", h.revokeJoinCode)
	mux.HandleFunc("POST /v1/join", h.join)
}

type groupJSON struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Currency  string    `json:"currency"`
	Role      string    `json:"role"` // the caller's role in this group
	CreatedAt time.Time `json:"created_at"`
}

type memberJSON struct {
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joined_at"`
}

type groupDetailJSON struct {
	groupJSON
	Members []memberJSON `json:"members"`
}

type createRequest struct {
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if n := utf8.RuneCountInString(req.Name); n < 1 || n > 100 {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "name must be 1-100 characters")
		return
	}
	if req.Currency != "CAD" && req.Currency != "USD" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "currency must be CAD or USD")
		return
	}

	// The group and its owner membership commit together: a group with no
	// members would be unreachable.
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		h.internalError(w, "begin tx", err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // no-op after Commit
	qtx := h.q.WithTx(tx)

	g, err := qtx.CreateGroup(r.Context(), db.CreateGroupParams{Name: req.Name, Currency: req.Currency, CreatedBy: user.ID})
	if err != nil {
		h.internalError(w, "create group", err)
		return
	}
	if err := qtx.AddMembership(r.Context(), db.AddMembershipParams{GroupID: g.ID, UserID: user.ID, Role: "owner"}); err != nil {
		h.internalError(w, "add owner membership", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.internalError(w, "commit", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, groupJSON{
		ID: g.ID, Name: g.Name, Currency: g.Currency, Role: "owner", CreatedAt: g.CreatedAt,
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	rows, err := h.q.ListGroupsForUser(r.Context(), user.ID)
	if err != nil {
		h.internalError(w, "list groups", err)
		return
	}
	out := make([]groupJSON, 0, len(rows))
	for _, g := range rows {
		out = append(out, groupJSON{ID: g.ID, Name: g.Name, Currency: g.Currency, Role: g.Role, CreatedAt: g.CreatedAt})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"groups": out})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	// A malformed id, an unknown group, and a group the caller is not in all
	// return the same 404, so ids cannot be probed for existence.
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		notFound(w)
		return
	}
	g, err := h.q.GetGroupForMember(r.Context(), db.GetGroupForMemberParams{ID: id, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		notFound(w)
		return
	}
	if err != nil {
		h.internalError(w, "get group", err)
		return
	}
	members, err := h.q.ListMembers(r.Context(), id)
	if err != nil {
		h.internalError(w, "list members", err)
		return
	}
	detail := groupDetailJSON{
		groupJSON: groupJSON{ID: g.ID, Name: g.Name, Currency: g.Currency, Role: g.Role, CreatedAt: g.CreatedAt},
		Members:   make([]memberJSON, 0, len(members)),
	}
	for _, m := range members {
		detail.Members = append(detail.Members, memberJSON{UserID: m.UserID, DisplayName: m.DisplayName, Role: m.Role, JoinedAt: m.JoinedAt})
	}
	httpx.WriteJSON(w, http.StatusOK, detail)
}

func notFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "group not found")
}

func (h *Handler) internalError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
}
