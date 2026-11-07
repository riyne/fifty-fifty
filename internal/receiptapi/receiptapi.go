// Package receiptapi implements receipt upload: POST /v1/groups/{id}/receipts.
//
// An upload is the start of the receipt pipeline (DESIGN.md section 8). It checks the
// caller, bounds the request, hardens the image (internal/receipts.HardenImage), puts
// the re-encoded bytes in the object store, and then, in one idempotent transaction,
// takes one unit of the user's daily quota, creates an expense in status `parsing`
// with no items, records the receipt that points at the stored object, and enqueues the
// receipt.parse job (internal/receiptparse) that turns the image into items. A worker
// that can run that job (it needs OCR and an extractor) moves the expense on; without
// one the job waits in the queue and the expense stays in `parsing`.
//
// Order of checks, and why:
//
//  1. Group membership, before anything else is looked at: a non-member learns nothing,
//     not even whether the request was well formed (404, like every group route).
//  2. Per-user rate limit, before the body is read: a client that floods the endpoint
//     costs us a 429, not a decode.
//  3. Idempotency-Key header and the multipart body, with size limits at every layer.
//  4. Hardening (CPU and memory heavy), behind a small semaphore: when it is full the
//     answer is 503 with Retry-After, never a queue of decoded images in memory.
//  5. The idempotent transaction: claim the key, take the quota, write the object, then
//     the expense and the receipt row. The object is written inside the transaction,
//     after the quota and before the row: a replay, a key reuse or an upload over the
//     quota writes nothing to the disk (a user at their limit cannot fill it), and a
//     committed receipt row always points at a stored object. A transaction that fails
//     after the write leaves a blob nothing points at (harmless, never swept).
//
// Quota: ConsumeReceiptQuota runs inside the idempotent transaction. A replay of a
// stored response consumes nothing, a rolled-back upload gives its unit back, and an
// upload over the limit is a 429 that is not stored under the key (a retry tomorrow
// with the same key must be able to succeed). The quota is a cost cap, not a ledger
// protection, so it has tests and no negative control.
package receiptapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/dbscope"
	"github.com/riyne/fifty-fifty/internal/httpx"
	"github.com/riyne/fifty-fifty/internal/idempotency"
	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/ratelimit"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/receipts"
)

// Defaults for Config.
const (
	DefaultDailyQuota    = 20
	DefaultMaxHardenings = 4
	DefaultReceiptTitle  = "Receipt"
	maxDescriptionRunes  = 200
	maxDescriptionBytes  = 1024
	multipartOverhead    = 1 << 20
	uploadReadWindow     = 60 * time.Second
	auditKindUploaded    = "receipt.uploaded"
)

// Config tunes a Handler. The zero value means the defaults.
type Config struct {
	// DailyQuota is how many receipts one user may upload per UTC day.
	DailyQuota int
	// MaxHardenings is how many uploads may be decoding at once; the rest get 503.
	MaxHardenings int
	// Limits bound the image. The zero value means receipts.DefaultImageLimits.
	Limits receipts.ImageLimits
}

// Handler serves the upload route.
type Handler struct {
	scope   *dbscope.DB
	store   objstore.Store
	limiter *ratelimit.Limiter
	cfg     Config
	sem     chan struct{}
}

// NewHandler returns a Handler. limiter is the per-user request rate limit.
func NewHandler(pool *pgxpool.Pool, store objstore.Store, limiter *ratelimit.Limiter, cfg Config) *Handler {
	if cfg.DailyQuota == 0 {
		cfg.DailyQuota = DefaultDailyQuota
	}
	if cfg.MaxHardenings == 0 {
		cfg.MaxHardenings = DefaultMaxHardenings
	}
	if cfg.Limits == (receipts.ImageLimits{}) {
		cfg.Limits = receipts.DefaultImageLimits
	}
	return &Handler{scope: dbscope.New(pool), store: store, limiter: limiter, cfg: cfg, sem: make(chan struct{}, cfg.MaxHardenings)}
}

// Register mounts the route on mux (inside the authenticated API).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/groups/{id}/receipts", h.upload)
}

type uploadJSON struct {
	ExpenseID   uuid.UUID `json:"expense_id"`
	ReceiptID   uuid.UUID `json:"receipt_id"`
	Status      string    `json:"status"`
	Version     int32     `json:"version"`
	ContentType string    `json:"content_type"`
	Width       int       `json:"width_px"`
	Height      int       `json:"height_px"`
	SizeBytes   int       `json:"size_bytes"`
}

// errQuota is returned from inside the transaction to roll it back without storing a
// response under the idempotency key.
var errQuota = errors.New("receiptapi: daily quota used")

func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	groupID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		groupNotFound(w)
		return
	}
	if _, err := h.scope.Reader(user.ID).GetGroupForMember(r.Context(), db.GetGroupForMemberParams{ID: groupID, UserID: user.ID}); errors.Is(err, pgx.ErrNoRows) {
		groupNotFound(w)
		return
	} else if err != nil {
		h.internalError(w, "get group", err)
		return
	}

	if ok, retry := h.limiter.Allow(user.ID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many uploads; slow down")
		return
	}
	key, err := idempotency.ParseKey(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// The api's server-wide read timeout suits small JSON bodies; a photo over a slow
	// mobile connection needs longer. The window is still bounded.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadWindow))
	description, data, ok := h.readForm(w, r)
	if !ok {
		return
	}

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, http.StatusServiceUnavailable, "busy", "too many uploads are being processed; retry shortly")
		return
	}
	img, err := receipts.HardenImage(data, h.cfg.Limits)
	data = nil // let the raw upload go before the rest of the work
	switch {
	case errors.Is(err, receipts.ErrTooLarge):
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
		return
	case errors.Is(err, receipts.ErrImage):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_image", err.Error())
		return
	case err != nil:
		h.internalError(w, "harden image", err)
		return
	}
	// The fingerprint is the hash of the re-encoded image (the key the object will have),
	// so it is a function of the pixels as stored, plus what else defines the request.
	objKey := objstore.Key(img.Data)
	fp := idempotency.Fingerprint("POST", "/v1/groups/{id}/receipts", groupID.String(), description, objKey)
	out, err := idempotency.Do(r.Context(), h.scope, user.ID, key, fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return h.insert(ctx, tx, user.ID, groupID, description, img)
	})
	switch {
	case errors.Is(err, idempotency.ErrKeyReuse):
		httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "Idempotency-Key was already used with a different request")
		return
	case errors.Is(err, errQuota):
		w.Header().Set("Retry-After", strconv.Itoa(secondsUntilUTCMidnight(time.Now())))
		httpx.WriteError(w, http.StatusTooManyRequests, "quota_exceeded",
			fmt.Sprintf("the daily limit of %d receipts is used; it resets at 00:00 UTC", h.cfg.DailyQuota))
		return
	case err != nil:
		h.internalError(w, "upload receipt", err)
		return
	}
	if out.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	_, _ = w.Write(out.Body)
}

// insert runs inside the idempotency transaction: quota, expense, receipt and audit
// event commit together or not at all.
func (h *Handler) insert(ctx context.Context, tx pgx.Tx, userID, groupID uuid.UUID, description string, img receipts.Image) (idempotency.Result, error) {
	q := db.New(tx)
	if _, err := q.ConsumeReceiptQuota(ctx, db.ConsumeReceiptQuotaParams{UserID: userID, DailyLimit: int32(h.cfg.DailyQuota)}); errors.Is(err, pgx.ErrNoRows) {
		return idempotency.Result{}, errQuota
	} else if err != nil {
		return idempotency.Result{}, fmt.Errorf("consume quota: %w", err)
	}
	// The object is written only now: after the idempotency claim and the quota, so a
	// replay, a key reuse or an upload over the quota writes nothing to the disk, and
	// before the receipt row, so a committed row always points at a stored object. If
	// the transaction fails after this, the blob stays (nothing points at it; harmless,
	// not swept).
	objKey, err := h.store.Put(ctx, img.Data)
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("store receipt image: %w", err)
	}
	e, err := q.CreateParsingExpense(ctx, db.CreateParsingExpenseParams{GroupID: groupID, PayerID: userID, Description: description})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("create expense: %w", err)
	}
	rc, err := q.CreateReceipt(ctx, db.CreateReceiptParams{
		ExpenseID: e.ID, GroupID: groupID, UploaderID: userID, ObjectKey: objKey,
		ContentType: img.ContentType, SizeBytes: int32(len(img.Data)), WidthPx: int32(img.Width), HeightPx: int32(img.Height),
	})
	if err != nil {
		return idempotency.Result{}, fmt.Errorf("create receipt: %w", err)
	}
	// The parse job is written in this transaction (no dual write): the receipt exists
	// exactly when its job does.
	if _, err := queue.Enqueue(ctx, tx, queue.NewJob{
		Kind: receiptparse.KindParse, Payload: receiptparse.Payload{ReceiptID: rc.ID}, MaxAttempts: receiptparse.ParseMaxAttempts,
	}); err != nil {
		return idempotency.Result{}, fmt.Errorf("enqueue parse job: %w", err)
	}
	audit, _ := json.Marshal(map[string]any{
		"expense_id": e.ID, "receipt_id": rc.ID, "content_type": img.ContentType,
		"size_bytes": len(img.Data), "width_px": img.Width, "height_px": img.Height,
	})
	if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{ActorUserID: userID, GroupID: groupID, Kind: auditKindUploaded, Data: audit}); err != nil {
		return idempotency.Result{}, fmt.Errorf("audit: %w", err)
	}
	body, err := json.Marshal(uploadJSON{
		ExpenseID: e.ID, ReceiptID: rc.ID, Status: e.Status, Version: e.Version,
		ContentType: img.ContentType, Width: img.Width, Height: img.Height, SizeBytes: len(img.Data),
	})
	if err != nil {
		return idempotency.Result{}, err
	}
	return idempotency.Result{Status: http.StatusAccepted, Body: append(body, '\n')}, nil
}

// readForm reads the multipart body: exactly one `file` part and at most one
// `description` part. Anything else is a 400, so the endpoint accepts one shape. It
// writes the error response itself and reports ok=false.
func (h *Handler) readForm(w http.ResponseWriter, r *http.Request) (description string, data []byte, ok bool) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "multipart/form-data" {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send multipart/form-data with a file part")
		return "", nil, false
	}
	max := h.cfg.Limits.MaxUploadBytes
	r.Body = http.MaxBytesReader(w, r.Body, int64(max)+multipartOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "malformed multipart body")
		return "", nil, false
	}
	tooLarge := func() {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the upload is larger than %d bytes", max))
	}
	var haveFile, haveDesc bool
	// No cap on the number of parts is needed: a valid form has at most two, so a third
	// is a second file, a second description or an unknown name, and each of those is
	// refused as it is read.
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var big *http.MaxBytesError
			if errors.As(err, &big) {
				tooLarge()
			} else {
				httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "malformed multipart body")
			}
			return "", nil, false
		}
		switch part.FormName() {
		case "file":
			if haveFile {
				httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "send exactly one file part")
				return "", nil, false
			}
			haveFile = true
			// One byte past the limit is enough for HardenImage to say "too large".
			data, err = io.ReadAll(io.LimitReader(part, int64(max)+1))
		case "description":
			if haveDesc {
				httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "send at most one description part")
				return "", nil, false
			}
			haveDesc = true
			var b []byte
			b, err = io.ReadAll(io.LimitReader(part, maxDescriptionBytes+1))
			if err == nil && len(b) > maxDescriptionBytes {
				httpx.WriteError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("description must be 1-%d characters", maxDescriptionRunes))
				return "", nil, false
			}
			description = string(b)
		default:
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "unknown form part; send file and optionally description")
			return "", nil, false
		}
		if err != nil {
			var big *http.MaxBytesError
			if errors.As(err, &big) {
				tooLarge()
			} else {
				httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "could not read the upload")
			}
			return "", nil, false
		}
	}
	if !haveFile {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "a file part is required")
		return "", nil, false
	}
	if !haveDesc {
		description = DefaultReceiptTitle
	}
	description = strings.TrimSpace(description)
	if n := utf8.RuneCountInString(description); n < 1 || n > maxDescriptionRunes || !utf8.ValidString(description) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("description must be 1-%d characters", maxDescriptionRunes))
		return "", nil, false
	}
	return description, data, true
}

func secondsUntilUTCMidnight(now time.Time) int {
	u := now.UTC()
	next := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
	return int((next.Sub(u) + time.Second - 1) / time.Second) // whole seconds, rounded up
}

func groupNotFound(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "group not found")
}

func (h *Handler) internalError(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
}
