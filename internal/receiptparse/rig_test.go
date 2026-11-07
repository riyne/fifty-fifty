package receiptparse_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"mime/multipart"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/receiptapi"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
	"github.com/riyne/fifty-fifty/internal/teststack"
	"github.com/riyne/fifty-fifty/internal/testutil"
	"github.com/riyne/fifty-fifty/internal/worker"
)

var ctx = context.Background()

// ---- fakes ----

type fakeOCR struct {
	mu       sync.Mutex
	text     string
	err      error
	calls    int
	gotImage []byte
	gotType  string
	// textFor, if set, decides the text from the image (a ref lookup in a test).
	textFor func(img []byte) string
}

func (f *fakeOCR) Text(ctx context.Context, img []byte, contentType string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotImage, f.gotType = img, contentType
	if f.err != nil {
		return "", f.err
	}
	if f.textFor != nil {
		return f.textFor(img), nil
	}
	return f.text, nil
}

type fakeExtractor struct {
	mu      sync.Mutex
	answer  string
	err     error
	model   string
	in, out int
	calls   int
	gotText string
	// onCall runs at the start of every call (a test cancels a context from it).
	onCall func()
	// answerFor, if set, decides the answer from the text sent.
	answerFor func(text string) string
}

func (f *fakeExtractor) Extract(ctx context.Context, text string) (receiptparse.Extraction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotText = text
	if f.onCall != nil {
		f.onCall()
	}
	if f.err != nil {
		return receiptparse.Extraction{}, f.err
	}
	a := f.answer
	if f.answerFor != nil {
		a = f.answerFor(text)
	}
	return receiptparse.Extraction{Answer: []byte(a), Model: f.model, InputTokens: f.in, OutputTokens: f.out}, nil
}

// ---- the rig: a stack, a group with a payer, and a way to run the handler once ----

type rig struct {
	s *teststack.Stack // s.Pool is the owner: it seeds and inspects
	// worker is the WORKER role (fifty_worker): the handlers, the sweep, the commit and the
	// worker loop under test run as it, as they do in cmd/worker.
	worker  *pgxpool.Pool
	store   *objstore.FS
	payer   string // token
	payerID uuid.UUID
	group   string
	ocr     *fakeOCR
	ex      *fakeExtractor
}

func newRig(t *testing.T, opts ...teststack.Option) *rig {
	t.Helper()
	// These tests upload far more than the default daily quota.
	opts = append([]teststack.Option{teststack.WithReceiptConfig(receiptapi.Config{DailyQuota: 10000})}, opts...)
	return newRigOn(t, teststack.New(t, opts...), testutil.WorkerPool(t))
}

func newRigOn(t *testing.T, s *teststack.Stack, worker *pgxpool.Pool) *rig {
	t.Helper()
	store, err := objstore.NewFS(s.ReceiptDir)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{s: s, worker: worker, store: store, payer: s.Token(t, "payer"), ocr: &fakeOCR{}, ex: &fakeExtractor{model: "fake-model", in: 120, out: 80}}
	r.group = s.CreateGroup(t, r.payer, "G")
	r.payerID = s.UserID(t, r.payer)
	return r
}

type upload struct {
	ExpenseID uuid.UUID `json:"expense_id"`
	ReceiptID uuid.UUID `json:"receipt_id"`
}

// upload sends a receipt through the real api and returns the ids.
func (r *rig) upload(t *testing.T, img []byte) upload {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormFile("file", "receipt.png")
	w.Write(img)
	mw.Close()
	resp := r.s.Do(t, r.payer, "POST", "/v1/groups/"+r.group+"/receipts", buf.String(),
		"Content-Type", mw.FormDataContentType(), "Idempotency-Key", uuid.NewString())
	if resp.Status != http.StatusAccepted {
		t.Fatalf("upload = %d: %s", resp.Status, resp.Body)
	}
	var u upload
	resp.JSON(t, &u)
	return u
}

// uploadSynth uploads the rendered synthetic receipt for ref.
func (r *rig) uploadSynth(t *testing.T, ref int) upload {
	t.Helper()
	img, err := synth.Generate(ref).Render(synth.RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r.upload(t, img)
}

func (r *rig) handler(cfg receiptparse.Config) worker.Handler {
	return receiptparse.Handler(r.worker, r.store, r.ocr, r.ex, cfg)
}

// runOnce runs the handler for a receipt as attempt n of max: phase one, then, if it
// returns a commit, phase two in a transaction (what the worker does with the queue's
// bookkeeping left out). It reports whether a commit ran.
func (r *rig) runOnce(t *testing.T, h worker.Handler, id uuid.UUID, attempt, max int) (committed bool, err error) {
	t.Helper()
	commit, err := h(ctx, claimed(id, attempt, max))
	if err != nil || commit == nil {
		return false, err
	}
	return true, r.commitIn(t, commit)
}

func (r *rig) commitIn(t *testing.T, commit worker.Commit) error {
	t.Helper()
	tx, err := r.worker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := commit(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func claimed(id uuid.UUID, attempt, max int) queue.Claimed {
	p, _ := json.Marshal(receiptparse.Payload{ReceiptID: id})
	return queue.Claimed{ID: uuid.New(), Kind: receiptparse.KindParse, Payload: p, Attempt: attempt, MaxAttempts: max}
}

// ---- reading state back ----

type expenseRow struct {
	Status   string
	Version  int
	Tax, Tip int64
	Items    int
	ItemSum  int64
}

func (r *rig) expense(t *testing.T, id uuid.UUID) expenseRow {
	t.Helper()
	var e expenseRow
	err := r.s.Pool.QueryRow(ctx, `SELECT e.status, e.version, e.tax_minor, e.tip_minor,
		(SELECT count(*) FROM expense_items i WHERE i.expense_id = e.id),
		(SELECT coalesce(sum(price_minor), 0) FROM expense_items i WHERE i.expense_id = e.id)
		FROM expenses e WHERE e.id = $1`, id).Scan(&e.Status, &e.Version, &e.Tax, &e.Tip, &e.Items, &e.ItemSum)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type receiptRow struct {
	Parsed                bool
	Outcome               string
	Reasons               []string
	OCRChars              *int
	Cards, Emails, Phones *int
	Model                 *string
	InTok, OutTok         *int
	PrintedTotal, Diff    *int64
}

func (r *rig) receipt(t *testing.T, id uuid.UUID) receiptRow {
	t.Helper()
	var rr receiptRow
	var parsedAt *time.Time
	var outcome *string
	err := r.s.Pool.QueryRow(ctx, `SELECT parsed_at, parse_outcome, parse_reasons, ocr_chars, redacted_cards, redacted_emails,
		redacted_phones, model, input_tokens, output_tokens, printed_total_minor, diff_minor FROM receipts WHERE id = $1`, id).
		Scan(&parsedAt, &outcome, &rr.Reasons, &rr.OCRChars, &rr.Cards, &rr.Emails, &rr.Phones, &rr.Model, &rr.InTok, &rr.OutTok, &rr.PrintedTotal, &rr.Diff)
	if err != nil {
		t.Fatal(err)
	}
	rr.Parsed = parsedAt != nil
	if outcome != nil {
		rr.Outcome = *outcome
	}
	return rr
}

func (r *rig) audits(t *testing.T, expense uuid.UUID) int {
	t.Helper()
	return r.s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE kind = $1 AND data->>'expense_id' = $2`, receiptparse.AuditParsed, expense.String())
}

func (r *rig) itemDescriptions(t *testing.T, expense uuid.UUID) []string {
	t.Helper()
	rows, err := r.s.Pool.Query(ctx, `SELECT description FROM expense_items WHERE expense_id = $1 ORDER BY position`, expense)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func eq[T comparable](p *T, want T) bool { return p != nil && *p == want }

// plantedCounts is how many of each kind of personal data the ground truth planted.
func plantedCounts(rc synth.Receipt) (cards, emails, phones int) {
	for _, p := range rc.Planted {
		switch p.Kind {
		case synth.Card:
			cards++
		case synth.Email:
			emails++
		case synth.Phone:
			phones++
		}
	}
	return
}
