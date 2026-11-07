// Package receiptparse is the receipt.parse job: it turns an uploaded receipt image into
// items, or into an expense a person has to fill in.
//
// The job is a two-phase queue handler (internal/worker):
//
//	phase one, no transaction open, may run more than once:
//	    read the receipt, fetch the image, OCR it, redact personal data, ask the extractor,
//	    validate and reconcile the answer (internal/receipts)
//	phase two, inside the transaction that also marks the job succeeded, exactly once:
//	    move the expense out of `parsing`, write the items, record what happened
//
// Phase one talks to services that can be slow, fail or be billed (OCR, the model), so a
// crash or a lost lease repeats it; phase two writes only to Postgres and is conditional
// on the expense still being `parsing`, so a repeat, a second job or the sweep (sweep.go)
// cannot write the items twice. The model's answer is untrusted: it is parsed against a
// strict schema and the decision to ask a person is arithmetic (receipts.Interpret).
//
// What happens to each kind of failure (DESIGN.md, "M6 plan decisions", decision 5):
//
//	the answer is invalid, or does not add up    -> needs_review at once (a retry would
//	                                                only draw another answer), with the
//	                                                items that were read when there are any
//	no image, no text after OCR and redaction    -> needs_review at once; the extractor is
//	                                                not called
//	OCR or the extractor fails (timeout, 5xx...) -> the attempt fails and is retried with
//	                                                backoff; on the LAST attempt the job
//	                                                commits needs_review with no items
//	                                                instead of failing, so the payer can type
//	                                                the receipt in (there is no failed status)
//	the context ends (lease lost, shutdown)      -> the attempt fails whatever the attempt
//	                                                number: that is not a verdict on the receipt
//
// Personal data: card numbers, email addresses and phone numbers are replaced by
// placeholders (receipts.Redact) before the text goes to the extractor. Names and
// addresses are not touched. Nothing about the content (OCR text, answer) is stored or
// logged; the receipt row gets counts, token usage and reason codes.
package receiptparse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/worker"
)

// KindParse is the job kind the upload enqueues.
const KindParse = "receipt.parse"

// ParseMaxAttempts is how many runs a parse job gets. The last one commits needs_review
// instead of failing. A placeholder, not tuned.
const ParseMaxAttempts = 4

// Payload is the payload of a KindParse job: an id, not data (payloads are copied into
// every retry and log line).
type Payload struct {
	ReceiptID uuid.UUID `json:"receipt_id"`
}

// Additional reasons a person has to look, beside the validator's (receipts.Reason*).
// The receipts table's check constraint lists exactly these and the validator's.
const (
	ReasonNoText      = "no_text"           // nothing readable came out of the image
	ReasonNoImage     = "image_unavailable" // the stored image is missing or damaged
	ReasonOCRFailed   = "ocr_failed"        // OCR failed on every attempt
	ReasonExtractFail = "extractor_failed"  // the extractor failed on every attempt
	ReasonAbandoned   = "parse_abandoned"   // the job died and the sweep gave up on it
)

// OCR reads text from an image. Implementations must honor ctx.
type OCR interface {
	Text(ctx context.Context, image []byte, contentType string) (string, error)
}

// Extractor turns receipt text into the model's raw answer (the JSON object that
// receipts.ParseExtraction accepts). It is untrusted output: the handler validates it.
type Extractor interface {
	Extract(ctx context.Context, text string) (Extraction, error)
}

// Extraction is what an Extractor returns.
type Extraction struct {
	Answer       []byte
	Model        string // as reported by the service; stored, at most 100 characters
	InputTokens  int
	OutputTokens int
}

// Config tunes the handler. Zero values take the defaults noted.
type Config struct {
	// MaxOCRChars bounds the text sent to the extractor (in characters). Receipts are a
	// few hundred; the cap bounds the cost of a hostile image full of text. Default 6000.
	MaxOCRChars int
	// OCRTimeout and ExtractTimeout bound one call each. Defaults 30 s and 60 s. Placeholders.
	OCRTimeout     time.Duration
	ExtractTimeout time.Duration
}

func (c *Config) setDefaults() {
	if c.MaxOCRChars == 0 {
		c.MaxOCRChars = 6000
	}
	if c.OCRTimeout == 0 {
		c.OCRTimeout = 30 * time.Second
	}
	if c.ExtractTimeout == 0 {
		c.ExtractTimeout = 60 * time.Second
	}
}

// result is everything phase one decided, and phase two writes.
type result struct {
	status  string // receipts.StatusClaiming or StatusNeedsReview
	reasons []string
	items   []receipts.Item
	tax     int64
	tip     int64

	printedTotal, diff *int64
	ocrChars           *int
	redactions         *receipts.Redactions
	model              string
	inputTokens        *int
	outputTokens       *int
	source             string // "worker" or "sweep", for the audit event
}

func needsReview(reason string) result {
	return result{status: receipts.StatusNeedsReview, reasons: []string{reason}, source: "worker"}
}

// Handler is the handler of KindParse.
func Handler(pool *pgxpool.Pool, store objstore.Store, ocr OCR, ex Extractor, cfg Config) worker.Handler {
	cfg.setDefaults()
	q := db.New(pool)
	return func(ctx context.Context, job queue.Claimed) (worker.Commit, error) {
		var p Payload
		if err := json.Unmarshal(job.Payload, &p); err != nil || p.ReceiptID == uuid.Nil {
			return nil, fmt.Errorf("receipt.parse: bad payload %q", job.Payload)
		}
		rc, err := q.GetReceiptForParse(ctx, p.ReceiptID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("receipt.parse: receipt %s does not exist", p.ReceiptID)
		}
		if err != nil {
			return nil, fmt.Errorf("read receipt: %w", err)
		}
		if rc.ParsedAt != nil || rc.ExpenseStatus != "parsing" {
			return nil, nil // already handled (a second job, the sweep): nothing to do
		}

		res, err := work(ctx, store, ocr, ex, cfg, rc)
		if err != nil {
			// A failed attempt. Retry unless this is the last one, and unless the
			// failure is the context ending, which says nothing about the receipt.
			var f *attemptError
			if ctx.Err() != nil || !errors.As(err, &f) || job.Attempt < job.MaxAttempts {
				return nil, err
			}
			slog.Warn("receipt.parse: giving up after the last attempt", "receipt_id", p.ReceiptID, "reason", f.reason, "err", err)
			res = needsReview(f.reason)
		}
		return commit(rc.ID, rc.ExpenseID, rc.GroupID, rc.UploaderID, res), nil
	}
}

// attemptError is a failure that is worth retrying and, on the last attempt, turns into
// needs_review with its reason.
type attemptError struct {
	reason string
	err    error
}

func (e *attemptError) Error() string { return e.err.Error() }
func (e *attemptError) Unwrap() error { return e.err }

// work is phase one. It returns a result for every outcome that is final, and an
// *attemptError for one that is worth retrying.
func work(ctx context.Context, store objstore.Store, ocr OCR, ex Extractor, cfg Config, rc db.GetReceiptForParseRow) (result, error) {
	img, err := store.Get(ctx, rc.ObjectKey)
	switch {
	case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrBadKey), errors.Is(err, objstore.ErrCorrupt):
		return needsReview(ReasonNoImage), nil // retrying will not bring the image back
	case err != nil:
		return result{}, &attemptError{ReasonNoImage, fmt.Errorf("read image: %w", err)}
	}

	octx, cancel := context.WithTimeout(ctx, cfg.OCRTimeout)
	text, err := ocr.Text(octx, img, rc.ContentType)
	cancel()
	if err != nil {
		return result{}, &attemptError{ReasonOCRFailed, fmt.Errorf("ocr: %w", err)}
	}
	redacted, counts := receipts.Redact(text)
	redacted = strings.TrimSpace(truncateRunes(redacted, cfg.MaxOCRChars))
	chars := utf8.RuneCountInString(redacted)
	base := result{ocrChars: &chars, redactions: &counts, source: "worker"}
	if redacted == "" {
		base.status, base.reasons = receipts.StatusNeedsReview, []string{ReasonNoText}
		return base, nil
	}

	ectx, cancel := context.WithTimeout(ctx, cfg.ExtractTimeout)
	got, err := ex.Extract(ectx, redacted)
	cancel()
	if err != nil {
		return result{}, &attemptError{ReasonExtractFail, fmt.Errorf("extract: %w", err)}
	}

	out := receipts.Interpret(got.Answer)
	base.status, base.reasons = out.Status, out.Reasons
	base.items, base.tax, base.tip = out.Items, out.TaxMinor, out.TipMinor
	if out.PrintedTotalMinor != 0 || len(out.Items) > 0 {
		pt, d := out.PrintedTotalMinor, out.DiffMinor
		base.printedTotal, base.diff = &pt, &d
	}
	base.model = truncateRunes(got.Model, 100)
	in, outTok := got.InputTokens, got.OutputTokens
	base.inputTokens, base.outputTokens = &in, &outTok
	return base, nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// commit is phase two. It runs in the completion transaction.
func commit(receiptID, expenseID, groupID, uploaderID uuid.UUID, res result) worker.Commit {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := apply(ctx, tx, receiptID, expenseID, groupID, uploaderID, res)
		return err
	}
}

// apply moves the expense out of `parsing` and records the run. It reports false, having
// written nothing, when the expense was no longer `parsing` (someone else got there
// first). The status change comes first: it is the guard, and it takes the row lock that
// serializes this against the sweep.
func apply(ctx context.Context, tx pgx.Tx, receiptID, expenseID, groupID, uploaderID uuid.UUID, res result) (bool, error) {
	q := db.New(tx)
	version, err := q.MarkExpenseParsed(ctx, db.MarkExpenseParsedParams{
		ID: expenseID, Status: res.status, TaxMinor: res.tax, TipMinor: res.tip,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("mark expense parsed: %w", err)
	}
	if len(res.items) > 0 {
		p := db.CreateExpenseItemsParams{ExpenseID: expenseID}
		for i, it := range res.items {
			p.Positions = append(p.Positions, int32(i))
			p.Descriptions = append(p.Descriptions, it.Description)
			p.Prices = append(p.Prices, it.PriceMinor)
		}
		if _, err := q.CreateExpenseItems(ctx, p); err != nil {
			return false, fmt.Errorf("insert items: %w", err)
		}
	}

	rec := db.RecordReceiptParseParams{ID: receiptID, Outcome: res.status, Reasons: res.reasons}
	if rec.Reasons == nil {
		rec.Reasons = []string{}
	}
	if res.ocrChars != nil {
		rec.OcrChars = pgtype.Int4{Int32: int32(*res.ocrChars), Valid: true}
	}
	if res.redactions != nil {
		rec.RedactedCards = pgtype.Int4{Int32: int32(res.redactions.Cards), Valid: true}
		rec.RedactedEmails = pgtype.Int4{Int32: int32(res.redactions.Emails), Valid: true}
		rec.RedactedPhones = pgtype.Int4{Int32: int32(res.redactions.Phones), Valid: true}
	}
	if res.model != "" {
		rec.Model = pgtype.Text{String: res.model, Valid: true}
	}
	if res.inputTokens != nil {
		rec.InputTokens = pgtype.Int4{Int32: int32(*res.inputTokens), Valid: true}
	}
	if res.outputTokens != nil {
		rec.OutputTokens = pgtype.Int4{Int32: int32(*res.outputTokens), Valid: true}
	}
	if res.printedTotal != nil {
		rec.PrintedTotalMinor = pgtype.Int8{Int64: *res.printedTotal, Valid: true}
	}
	if res.diff != nil {
		rec.DiffMinor = pgtype.Int8{Int64: *res.diff, Valid: true}
	}
	n, err := q.RecordReceiptParse(ctx, rec)
	if err != nil {
		return false, fmt.Errorf("record parse: %w", err)
	}
	if n != 1 {
		// The expense was `parsing`, so the receipt must have been waiting.
		return false, fmt.Errorf("receipt %s was already recorded as parsed while its expense was parsing", receiptID)
	}

	data, _ := json.Marshal(map[string]any{
		"expense_id": expenseID, "receipt_id": receiptID, "outcome": res.status, "reasons": rec.Reasons,
		"items": len(res.items), "version": version, "source": res.source,
	})
	if err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		ActorUserID: uploaderID, GroupID: groupID, Kind: AuditParsed, Data: data,
	}); err != nil {
		return false, fmt.Errorf("audit: %w", err)
	}
	return true, nil
}

// AuditParsed is the audit event kind written when an expense leaves `parsing`.
const AuditParsed = "receipt.parsed"
