package settlements

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/httpx"
	"github.com/riyne/fifty-fifty/internal/paysig"
)

const (
	// WebhookPath is where the payment provider sends events.
	WebhookPath = "/v1/webhooks/payments"
	// WebhookTolerance is how far a signature's timestamp may be from now, either way.
	// It bounds how long a captured request stays replayable; inside the window a replay
	// verifies, and is harmless because resolving a settlement is idempotent.
	WebhookTolerance = 5 * time.Minute
	// maxWebhookBytes bounds a request body, read before anything else is done with it.
	maxWebhookBytes = 1 << 16
)

// WebhookHandler receives payment-provider events. It is the one endpoint that is NOT
// behind user authentication: the caller is the provider, who proves it with an HMAC
// signature over the body (internal/paysig). Anything unsigned or wrongly signed is
// refused before the body is parsed, and nothing in the body is trusted for anything
// but which settlement it names and what happened to it.
//
// The handler only ever calls Resolve, the same function execute and reconcile use, so
// whichever of them (or a duplicate, or an out-of-order event) gets there first, the
// settlement is posted at most once. Events arrive duplicated and out of order; that
// is expected, not an error, and gets 200 so the provider does not retry.
type WebhookHandler struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	secret []byte
}

// NewWebhookHandler returns the handler. An empty secret is refused: it would make
// every signature forgeable.
func NewWebhookHandler(pool *pgxpool.Pool, secret []byte) (*WebhookHandler, error) {
	if len(secret) == 0 {
		return nil, paysig.ErrNoSecret
	}
	return &WebhookHandler{pool: pool, q: db.New(pool), secret: secret}, nil
}

// event is the part of the provider's webhook body that is used.
type event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Payment struct {
			IdempotencyKey string  `json:"idempotency_key"`
			Reference      string  `json:"reference"`
			From           string  `json:"from"`
			To             string  `json:"to"`
			AmountMinor    int64   `json:"amount_minor"`
			Currency       string  `json:"currency"`
			Status         string  `json:"status"`
			FailureCode    *string `json:"failure_code"`
		} `json:"payment"`
	} `json:"data"`
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "request body is too large")
		return
	}
	// One answer for every way a signature can be wrong, so the response teaches a
	// forger nothing (the reason goes to the log, not the wire).
	if err := paysig.Verify(h.secret, r.Header.Get(paysig.Header), body, time.Now(), WebhookTolerance); err != nil {
		slog.Warn("payment webhook refused", "reason", err.Error())
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_signature", "invalid signature")
		return
	}

	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "body is not a payment event")
		return
	}
	p := ev.Data.Payment
	var outcome Outcome
	switch ev.Type {
	case "payment.succeeded":
		if p.Status != "succeeded" {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "event type and payment status disagree")
			return
		}
		outcome = Success()
	case "payment.failed":
		if p.Status != "failed" {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "event type and payment status disagree")
			return
		}
		code := "declined"
		if p.FailureCode != nil && *p.FailureCode != "" {
			code = *p.FailureCode
			if rs := []rune(code); len(rs) > 100 {
				code = string(rs[:100])
			}
		}
		outcome = Failure(code)
	default:
		// payment.processing carries nothing a settlement does not already know, and a
		// provider adds event types over time: acknowledge, do nothing.
		h.ack(w, "ignored")
		return
	}

	// Our payments are keyed and referenced by the settlement id. Anything else is not
	// ours (or not for us): acknowledged so the provider stops sending it.
	id, err := uuid.Parse(p.IdempotencyKey)
	if err != nil || p.Reference != p.IdempotencyKey {
		slog.Warn("payment webhook for an unrecognized payment", "event", ev.ID)
		h.ack(w, "ignored")
		return
	}

	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		h.fail(w, "begin", err)
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // no-op after Commit

	res, err := h.resolve(r, tx, id, p.AmountMinor, p.Currency, p.From, p.To, outcome)
	if err != nil {
		h.fail(w, "resolve", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.fail(w, "commit", err)
		return
	}
	h.ack(w, res)
}

// resolve checks that the event describes the settlement it names, then resolves it. An
// event that names a settlement but disagrees about the money or the parties is not
// applied and is recorded for a person to look at.
func (h *WebhookHandler) resolve(r *http.Request, tx pgx.Tx, id uuid.UUID, amount int64, currency, from, to string, o Outcome) (string, error) {
	q := h.q.WithTx(tx)
	s, err := q.GetSettlementForExecution(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("payment webhook for an unknown settlement", "settlement", id)
		return "ignored", nil
	}
	if err != nil {
		return "", err
	}
	if amount != s.AmountMinor || currency != s.Currency || from != s.FromMember.String() || to != s.ToMember.String() {
		data, _ := json.Marshal(map[string]any{"settlement_id": id, "event_amount_minor": amount, "event_currency": currency, "settlement_amount_minor": s.AmountMinor})
		if err := q.InsertAuditEvent(r.Context(), db.InsertAuditEventParams{
			ActorUserID: s.FromMember, GroupID: s.GroupID, Kind: "settlement.webhook_mismatch", Data: data,
		}); err != nil {
			return "", err
		}
		return "ignored", nil
	}
	res, err := Resolve(r.Context(), tx, id, o)
	if err != nil {
		return "", err
	}
	return res.Action.String(), nil
}

func (h *WebhookHandler) ack(w http.ResponseWriter, result string) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"received": true, "result": result})
}

// fail answers 500 so a provider that retries will retry; nothing was committed.
func (h *WebhookHandler) fail(w http.ResponseWriter, what string, err error) {
	slog.Error("payment webhook", "step", what, "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
}
