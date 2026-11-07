package receiptparse_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
)

// ---- helpers shared with the parse tests ----

func (r *rig) groupID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(r.group)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// sweep runs one sweep restricted to the rig's group (the shared database holds other
// tests' receipts).
func (r *rig) sweep(t *testing.T, cfg receiptparse.SweepConfig) {
	t.Helper()
	g := r.groupID(t)
	cfg.Group = &g
	commit, err := receiptparse.SweepHandler(r.worker, cfg)(ctx, queue.Claimed{Kind: receiptparse.KindSweep})
	if err != nil || commit != nil {
		t.Fatalf("sweep: commit=%v err=%v", commit != nil, err)
	}
}

func (r *rig) ageReceipt(t *testing.T, id uuid.UUID) {
	t.Helper()
	r.s.Exec(t, `UPDATE receipts SET created_at = now() - interval '1 hour' WHERE id = $1`, id)
}

// killJobs removes the receipt's parse jobs: no live job.
func (r *rig) killJobs(t *testing.T, id uuid.UUID) {
	t.Helper()
	r.s.Exec(t, `DELETE FROM jobs WHERE kind = 'receipt.parse' AND payload->>'receipt_id' = $1`, id.String())
}

func (r *rig) setJobState(t *testing.T, id uuid.UUID, state string) {
	t.Helper()
	switch state {
	case "dead", "succeeded":
		r.s.Exec(t, `UPDATE jobs SET state = $2, finished_at = now() WHERE kind = 'receipt.parse' AND payload->>'receipt_id' = $1`, id.String(), state)
	case "running":
		r.s.Exec(t, `UPDATE jobs SET state = 'running', attempts = 1, lease_token = gen_random_uuid(), lease_expires_at = now() + interval '1 hour', run_at = now() + interval '1 hour'
		             WHERE kind = 'receipt.parse' AND payload->>'receipt_id' = $1`, id.String())
	default:
		t.Fatalf("state %q", state)
	}
}

func (r *rig) auditSource(t *testing.T, expense uuid.UUID) string {
	t.Helper()
	var src string
	if err := r.s.Pool.QueryRow(ctx, `SELECT data->>'source' FROM audit_events WHERE kind = $1 AND data->>'expense_id' = $2`, receiptparse.AuditParsed, expense.String()).Scan(&src); err != nil {
		t.Fatal(err)
	}
	return src
}

func (r *rig) abandoned(t *testing.T, u upload) bool {
	t.Helper()
	e, rc := r.expense(t, u.ExpenseID), r.receipt(t, u.ReceiptID)
	if e.Status == "needs_review" {
		if e.Items != 0 || e.Version != 2 || !rc.Parsed || rc.Outcome != "needs_review" || strings.Join(rc.Reasons, ",") != receiptparse.ReasonAbandoned || r.audits(t, u.ExpenseID) != 1 {
			t.Fatalf("a swept receipt is not recorded properly: %+v %+v, %d audit events", e, rc, r.audits(t, u.ExpenseID))
		}
		if src := r.auditSource(t, u.ExpenseID); src != "sweep" {
			t.Fatalf("audit source %q, want sweep", src)
		}
		return true
	}
	if e.Status != "parsing" || e.Version != 1 || rc.Parsed || r.audits(t, u.ExpenseID) != 0 {
		t.Fatalf("an untouched receipt changed: %+v %+v", e, rc)
	}
	return false
}

// ---- what the sweep takes and what it leaves ----

func TestSweep_TakesOnlyReceiptsNoLiveJobWillFinish(t *testing.T) {
	r := newRig(t)
	cfg := receiptparse.SweepConfig{StuckAfter: 10 * time.Minute}
	mk := func(old bool, job string) upload {
		u := r.uploadSynth(t, 1)
		if old {
			r.ageReceipt(t, u.ReceiptID)
		}
		switch job {
		case "none":
			r.killJobs(t, u.ReceiptID)
		case "queued":
		default:
			r.setJobState(t, u.ReceiptID, job)
		}
		return u
	}
	for _, tc := range []struct {
		name string
		old  bool
		job  string
		want bool // swept
	}{
		{"old, no job", true, "none", true},
		{"old, dead job", true, "dead", true},
		{"old, succeeded job but still parsing", true, "succeeded", true},
		{"old, queued job", true, "queued", false},
		{"old, running job", true, "running", false},
		{"young, no job", false, "none", false},
		{"young, dead job", false, "dead", false},
	} {
		u := mk(tc.old, tc.job)
		r.sweep(t, cfg)
		if got := r.abandoned(t, u); got != tc.want {
			t.Errorf("%s: swept=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSweep_LeavesExpensesThatAreNotParsing(t *testing.T) {
	r := newRig(t)
	truth := r.uploadSynth(t, 2)
	r.ocr.text, r.ex.answer = "text", `{"items":[{"description":"a","line_total_minor":100}],"total_minor":100}`
	if _, err := r.runOnce(t, r.handler(receiptparse.Config{}), truth.ReceiptID, 1, maxAttempts); err != nil {
		t.Fatal(err)
	}
	r.killJobs(t, truth.ReceiptID)
	r.ageReceipt(t, truth.ReceiptID)
	r.sweep(t, receiptparse.SweepConfig{StuckAfter: time.Minute})
	if e := r.expense(t, truth.ExpenseID); e.Status != "claiming" || e.Items != 1 || e.Version != 2 {
		t.Errorf("a parsed expense was touched: %+v", e)
	}
	if rc := r.receipt(t, truth.ReceiptID); rc.Outcome != "claiming" {
		t.Errorf("receipt %+v", rc)
	}
}

// A receipt already recorded as parsed is never taken, whatever else is true of it.
func TestSweep_LeavesReceiptsAlreadyRecordedAsParsed(t *testing.T) {
	r := newRig(t)
	u := r.uploadSynth(t, 2)
	r.killJobs(t, u.ReceiptID)
	r.ageReceipt(t, u.ReceiptID)
	r.s.Exec(t, `UPDATE receipts SET parsed_at = now(), parse_outcome = 'needs_review' WHERE id = $1`, u.ReceiptID)
	r.sweep(t, receiptparse.SweepConfig{StuckAfter: time.Minute})
	if e := r.expense(t, u.ExpenseID); e.Status != "parsing" || e.Version != 1 || r.audits(t, u.ExpenseID) != 0 {
		t.Errorf("a recorded receipt was swept: %+v", e)
	}
}

func TestSweep_IsIdempotentAndBoundedByGroup(t *testing.T) {
	r := newRig(t)
	other := newRig(t)
	mine := r.uploadSynth(t, 3)
	theirs := other.uploadSynth(t, 3)
	for _, x := range []struct {
		r *rig
		u upload
	}{{r, mine}, {other, theirs}} {
		x.r.killJobs(t, x.u.ReceiptID)
		x.r.ageReceipt(t, x.u.ReceiptID)
	}
	for i := 0; i < 3; i++ {
		r.sweep(t, receiptparse.SweepConfig{StuckAfter: time.Minute})
	}
	if !r.abandoned(t, mine) {
		t.Error("the group's own stuck receipt was not swept")
	}
	if other.abandoned(t, theirs) {
		t.Error("another group's receipt was swept by a sweep restricted to this group")
	}
	// Running it again changed nothing further (abandoned() checks version 2 and one audit event).
	r.sweep(t, receiptparse.SweepConfig{StuckAfter: time.Minute})
	r.abandoned(t, mine)
}

// More stuck receipts than one batch: the handler keeps going until none are left.
func TestSweep_WorksThroughMoreThanOneBatch(t *testing.T) {
	r := newRig(t)
	var all []upload
	for i := 0; i < 7; i++ {
		u := r.uploadSynth(t, 4)
		r.killJobs(t, u.ReceiptID)
		r.ageReceipt(t, u.ReceiptID)
		all = append(all, u)
	}
	r.sweep(t, receiptparse.SweepConfig{StuckAfter: time.Minute, Batch: 2})
	for _, u := range all {
		if !r.abandoned(t, u) {
			t.Errorf("receipt %s not swept", u.ReceiptID)
		}
	}
}

// Two sweeps at once take different receipts (SKIP LOCKED) and every receipt is
// recorded exactly once.
func TestSweep_ConcurrentSweepsRecordEachReceiptOnce(t *testing.T) {
	r := newRig(t)
	var all []upload
	for i := 0; i < 12; i++ {
		u := r.uploadSynth(t, 5)
		r.killJobs(t, u.ReceiptID)
		r.ageReceipt(t, u.ReceiptID)
		all = append(all, u)
	}
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() { r.sweep(t, receiptparse.SweepConfig{StuckAfter: time.Minute, Batch: 3}); done <- struct{}{} }()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	r.sweep(t, receiptparse.SweepConfig{StuckAfter: time.Minute}) // anything a concurrent sweep skipped
	for _, u := range all {
		if !r.abandoned(t, u) {
			t.Errorf("receipt %s not swept", u.ReceiptID)
		}
	}
}
