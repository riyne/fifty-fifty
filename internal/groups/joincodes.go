package groups

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/httpx"
	"github.com/riyne/fifty-fifty/internal/idempotency"
	"github.com/riyne/fifty-fifty/internal/joincode"
)

const (
	defaultMaxUses  = 5
	maxMaxUses      = 50
	defaultTTLHours = 72
	maxTTLHours     = 168
)

type joinCodeJSON struct {
	ID        uuid.UUID  `json:"id"`
	MaxUses   int32      `json:"max_uses"`
	Uses      int32      `json:"uses"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at"`
	CreatedAt time.Time  `json:"created_at"`
	Status    string     `json:"status"` // active | exhausted | expired | revoked
}

// createdJoinCodeJSON is the only place the plaintext code ever appears.
type createdJoinCodeJSON struct {
	ID        uuid.UUID `json:"id"`
	Code      string    `json:"code"`
	MaxUses   int32     `json:"max_uses"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// requireOwner resolves the {id} path value and checks the caller owns the
// group. A malformed id and a group the caller cannot see both give 404 (never
// leaking existence); a visible group where the caller is merely a member gives
// 403. It writes the response itself when it returns ok = false.
func (h *Handler) requireOwner(w http.ResponseWriter, r *http.Request) (user db.User, groupID uuid.UUID, ok bool) {
	user, _ = auth.UserFrom(r.Context())
	groupID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		notFound(w)
		return user, uuid.Nil, false
	}
	g, err := h.scope.Reader(user.ID).GetGroupForMember(r.Context(), db.GetGroupForMemberParams{ID: groupID, UserID: user.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		notFound(w)
		return user, uuid.Nil, false
	}
	if err != nil {
		h.internalError(w, "get group for owner check", err)
		return user, uuid.Nil, false
	}
	if g.Role != "owner" {
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "only the group owner can do this")
		return user, uuid.Nil, false
	}
	return user, groupID, true
}

type createJoinCodeRequest struct {
	MaxUses        *int `json:"max_uses"`
	ExpiresInHours *int `json:"expires_in_hours"`
}

func (h *Handler) createJoinCode(w http.ResponseWriter, r *http.Request) {
	user, groupID, ok := h.requireOwner(w, r)
	if !ok {
		return
	}

	var req createJoinCodeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil && !errors.Is(err, httpx.ErrEmptyBody) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	maxUses, ttl := defaultMaxUses, defaultTTLHours
	if req.MaxUses != nil {
		maxUses = *req.MaxUses
	}
	if req.ExpiresInHours != nil {
		ttl = *req.ExpiresInHours
	}
	if maxUses < 1 || maxUses > maxMaxUses {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "max_uses must be between 1 and "+strconv.Itoa(maxMaxUses))
		return
	}
	if ttl < 1 || ttl > maxTTLHours {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "expires_in_hours must be between 1 and "+strconv.Itoa(maxTTLHours))
		return
	}

	code := joincode.Generate()
	tx, err := h.scope.Begin(r.Context(), user.ID)
	if err != nil {
		h.internalError(w, "begin tx", err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // no-op after Commit
	qtx := db.New(tx)

	jc, err := qtx.CreateJoinCode(r.Context(), db.CreateJoinCodeParams{
		GroupID: groupID, CodeHash: joincode.Hash(code), CreatedBy: user.ID,
		MaxUses: int32(maxUses), TtlHours: int32(ttl),
	})
	if err != nil {
		h.internalError(w, "create join code", err)
		return
	}
	if err := h.audit(r.Context(), qtx, user.ID, groupID, "join_code.created", map[string]any{"join_code_id": jc.ID}); err != nil {
		h.internalError(w, "audit join_code.created", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.internalError(w, "commit", err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, createdJoinCodeJSON{
		ID: jc.ID, Code: joincode.Display(code), MaxUses: jc.MaxUses, ExpiresAt: jc.ExpiresAt, CreatedAt: jc.CreatedAt,
	})
}

func (h *Handler) listJoinCodes(w http.ResponseWriter, r *http.Request) {
	user, groupID, ok := h.requireOwner(w, r)
	if !ok {
		return
	}
	page, err := httpx.ParsePage(r)
	if err != nil {
		httpx.WriteBadPage(w)
		return
	}
	afterID, afterAt, fetch := page.QueryArgs()
	rows, err := h.scope.Reader(user.ID).ListJoinCodes(r.Context(), db.ListJoinCodesParams{
		GroupID: groupID, AfterID: afterID, AfterCreatedAt: afterAt, PageSize: fetch,
	})
	if err != nil {
		h.internalError(w, "list join codes", err)
		return
	}
	rows, next := httpx.Trim(rows, page.Limit, func(c db.ListJoinCodesRow) (time.Time, uuid.UUID) { return c.CreatedAt, c.ID })
	out := make([]joinCodeJSON, 0, len(rows))
	for _, c := range rows {
		out = append(out, joinCodeJSON{
			ID: c.ID, MaxUses: c.MaxUses, Uses: c.Uses, ExpiresAt: c.ExpiresAt,
			RevokedAt: c.RevokedAt, CreatedAt: c.CreatedAt, Status: c.Status,
		})
	}
	resp := map[string]any{"join_codes": out}
	if next != "" {
		resp["next_cursor"] = next
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) revokeJoinCode(w http.ResponseWriter, r *http.Request) {
	user, groupID, ok := h.requireOwner(w, r)
	if !ok {
		return
	}
	codeID, err := uuid.Parse(r.PathValue("codeId"))
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "join code not found")
		return
	}

	tx, err := h.scope.Begin(r.Context(), user.ID)
	if err != nil {
		h.internalError(w, "begin tx", err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // no-op after Commit
	qtx := db.New(tx)

	// The UPDATE is scoped to (code id AND the group already authorized above),
	// so a code id belonging to another group matches nothing.
	n, err := qtx.RevokeJoinCode(r.Context(), db.RevokeJoinCodeParams{ID: codeID, GroupID: groupID})
	if err != nil {
		h.internalError(w, "revoke join code", err)
		return
	}
	if n == 0 {
		// Either already revoked (fine, DELETE is idempotent) or not in this group.
		exists, err := qtx.JoinCodeExistsInGroup(r.Context(), db.JoinCodeExistsInGroupParams{ID: codeID, GroupID: groupID})
		if err != nil {
			h.internalError(w, "check join code", err)
			return
		}
		if !exists {
			httpx.WriteError(w, http.StatusNotFound, "not_found", "join code not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.audit(r.Context(), qtx, user.ID, groupID, "join_code.revoked", map[string]any{"join_code_id": codeID}); err != nil {
		h.internalError(w, "audit join_code.revoked", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.internalError(w, "commit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type joinRequest struct {
	Code string `json:"code"`
}

// join redeems a code. Order matters: rate limit first (so guessing is
// throttled even when requests are malformed), then cheap validation, then the
// idempotent transaction.
func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	if ok, retry := h.joinLimiter.Allow(user.ID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many join attempts; slow down")
		return
	}
	key, err := idempotency.ParseKey(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var req joinRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	code, err := joincode.Normalize(req.Code)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "code must be 12 characters, e.g. ABCD-EFGH-JKMN")
		return
	}
	hash := joincode.Hash(code)

	// Fingerprint the normalized code's hash, not the raw body, so "abcd-efgh-jkmn"
	// and "ABCDEFGHJKMN" count as the same request, and no plaintext code is
	// derivable from the stored fingerprint beyond what code_hash already allows.
	fp := idempotency.Fingerprint("POST", "/v1/join", string(hash))
	out, err := idempotency.Do(r.Context(), h.scope, user.ID, key, fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return h.redeem(ctx, tx, user, hash)
	})
	if errors.Is(err, idempotency.ErrKeyReuse) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "Idempotency-Key was already used with a different request")
		return
	}
	if err != nil {
		h.internalError(w, "join", err)
		return
	}
	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	_, _ = w.Write(out.Body)
}

// redeem runs inside the idempotency transaction. It writes nothing unless the
// join succeeds, so its 404 result is safe to store.
func (h *Handler) redeem(ctx context.Context, tx pgx.Tx, user db.User, hash []byte) (idempotency.Result, error) {
	q := db.New(tx)

	// One database function does the lock, the membership insert and the use count (see
	// migration 00016). Not found, revoked, expired and exhausted are all "no row".
	jc, err := q.RedeemJoinCode(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return jsonResult(http.StatusNotFound, errBody("invalid_join_code", "join code is invalid, expired, revoked, or fully used")), nil
	}
	if err != nil {
		return idempotency.Result{}, err
	}

	// Already a member: succeed without spending a use.
	status := http.StatusOK
	if jc.Added {
		status = http.StatusCreated
		if err := h.audit(ctx, q, user.ID, jc.GroupID, "member.joined", map[string]any{"join_code_id": jc.JoinCodeID}); err != nil {
			return idempotency.Result{}, err
		}
	}

	g, err := q.GetGroupForMember(ctx, db.GetGroupForMemberParams{ID: jc.GroupID, UserID: user.ID})
	if err != nil {
		return idempotency.Result{}, err
	}
	return jsonResult(status, groupJSON{ID: g.ID, Name: g.Name, Currency: g.Currency, Role: g.Role, CreatedAt: g.CreatedAt}), nil
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

func jsonResult(status int, v any) idempotency.Result {
	b, err := json.Marshal(v)
	if err != nil {
		// Only reachable with an unmarshalable type, i.e. a programming error.
		panic("groups: marshal response: " + err.Error())
	}
	return idempotency.Result{Status: status, Body: append(b, '\n')}
}
