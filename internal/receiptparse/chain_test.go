package receiptparse_test

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/receiptapi"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
	"github.com/riyne/fifty-fifty/internal/settlerig"
	"github.com/riyne/fifty-fifty/internal/teststack"
	"github.com/riyne/fifty-fifty/internal/worker"
)

// chainRig is the whole path on a scratch database: the api writes the upload and its
// job in one transaction, a real worker claims the job, the handler runs, the queue
// completes it. A scratch database because the worker claims every job of its kinds.
// scratchURLs remembers the worker-role URL of each chain rig's scratch database, for tests
// that start other processes against it.
var scratchURLs sync.Map

func scratchWorkerURL(t *testing.T, r *rig) string {
	t.Helper()
	v, ok := scratchURLs.Load(r)
	if !ok {
		t.Fatal("not a chain rig")
	}
	return v.(string)
}

func chainRig(t *testing.T) *rig {
	t.Helper()
	ownerURL, appURL := os.Getenv("TEST_OWNER_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		t.Skip("TEST_OWNER_DATABASE_URL and TEST_DATABASE_URL are required")
	}
	sc, err := settlerig.NewScratch(ctx, ownerURL, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sc.Drop(context.Background()); err != nil {
			t.Errorf("drop %s: %v", sc.Name, err)
		}
	})
	workerPool, err := pgxpool.New(ctx, sc.WorkerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(workerPool.Close) // registered after the Drop above, so it runs before it
	r := newRigOn(t, teststack.NewOn(t, sc.Pool, sc.Owner, teststack.WithReceiptConfig(receiptapi.Config{DailyQuota: 1000})), workerPool)
	scratchURLs.Store(r, sc.WorkerURL)
	t.Cleanup(func() { scratchURLs.Delete(r) })
	return r
}

// startWorker runs a fast worker with the given handlers and returns a stop function.
func (r *rig) startWorker(t *testing.T, handlers map[string]worker.Handler) (stop func()) {
	t.Helper()
	return r.startWorkerCfg(t, handlers, nil)
}

// startWorkerCfg is startWorker with a chance to change the config.
func (r *rig) startWorkerCfg(t *testing.T, handlers map[string]worker.Handler, mod func(*worker.Config)) (stop func()) {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	cfg := worker.Config{
		Pool: r.worker, Handlers: handlers, Concurrency: 4,
		PollInterval: 10 * time.Millisecond, Lease: 5 * time.Second, ReapInterval: 100 * time.Millisecond,
		ShutdownGrace: 2 * time.Second, Backoff: queue.Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mod != nil {
		mod(&cfg)
	}
	go func() { done <- worker.Run(runCtx, cfg) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("worker did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

type parseJob struct {
	state    string
	attempts int
}

func (r *rig) parseJob(t *testing.T, receipt uuid.UUID) parseJob {
	t.Helper()
	var j parseJob
	if err := r.s.Pool.QueryRow(ctx, `SELECT state, attempts FROM jobs WHERE kind = 'receipt.parse' AND payload->>'receipt_id' = $1`, receipt.String()).Scan(&j.state, &j.attempts); err != nil {
		t.Fatal(err)
	}
	return j
}

func (r *rig) waitJob(t *testing.T, receipt uuid.UUID, state string) parseJob {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if j := r.parseJob(t, receipt); j.state == state {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job never reached %q: %+v", state, r.parseJob(t, receipt))
	return parseJob{}
}

// answersByRef makes the fakes behave like the mock LLM's fixture mode: the OCR returns the
// ground-truth text of the receipt whose image it is given, and the extractor answers with
// the ground truth for the REF token it finds in the text.
func (r *rig) answersByRef(t *testing.T, ups map[uuid.UUID]int) {
	t.Helper()
	truths := map[string]synth.Receipt{} // by object key
	for id, ref := range ups {
		var key string
		if err := r.s.Pool.QueryRow(ctx, `SELECT object_key FROM receipts WHERE id = $1`, id).Scan(&key); err != nil {
			t.Fatal(err)
		}
		truths[key] = synth.Generate(ref)
	}
	r.ocr.textFor = func(img []byte) string { return truths[objstore.Key(img)].Text() }
	ref := regexp.MustCompile(`REF (\d+)`)
	r.ex.answerFor = func(text string) string {
		m := ref.FindStringSubmatch(text)
		if m == nil {
			return "no REF token"
		}
		n, _ := strconv.Atoi(m[1])
		return synth.Generate(n).Answer()
	}
}

func TestChain_UploadsBecomeClaimableItemsThroughTheWorker(t *testing.T) {
	r := chainRig(t)
	ups := map[uuid.UUID]int{}
	exp := map[uuid.UUID]upload{}
	for ref := 1; ref <= 8; ref++ {
		u := r.uploadSynth(t, ref)
		ups[u.ReceiptID] = ref
		exp[u.ReceiptID] = u
	}
	r.answersByRef(t, ups)
	r.startWorker(t, map[string]worker.Handler{receiptparse.KindParse: r.handler(receiptparse.Config{})})

	for id, ref := range ups {
		j := r.waitJob(t, id, "succeeded")
		truth := synth.Generate(ref)
		e := r.expense(t, exp[id].ExpenseID)
		if j.attempts != 1 || e.Status != "claiming" || e.Version != 2 || e.ItemSum+e.Tax+e.Tip != truth.TotalMinor {
			t.Errorf("ref %d: job %+v, expense %+v, want total %d", ref, j, e, truth.TotalMinor)
		}
	}
}

// The extractor never works: the job is retried with backoff, and the last attempt hands
// the expense to the payer. The job itself ends succeeded: giving up is a result, not a crash.
func TestChain_AnExtractorThatNeverWorksEndsInNeedsReview(t *testing.T) {
	r := chainRig(t)
	r.ocr.text = "some text"
	r.ex.err = errors.New("503 from the model")
	u := r.uploadSynth(t, 1)
	r.startWorker(t, map[string]worker.Handler{receiptparse.KindParse: r.handler(receiptparse.Config{})})

	j := r.waitJob(t, u.ReceiptID, "succeeded")
	e, rc := r.expense(t, u.ExpenseID), r.receipt(t, u.ReceiptID)
	if j.attempts != receiptparse.ParseMaxAttempts || e.Status != "needs_review" || e.Items != 0 || strings.Join(rc.Reasons, ",") != receiptparse.ReasonExtractFail {
		t.Fatalf("job %+v, expense %+v, receipt %+v", j, e, rc)
	}
	if r.ex.calls != receiptparse.ParseMaxAttempts {
		t.Errorf("%d extractor calls, want one per attempt (%d)", r.ex.calls, receiptparse.ParseMaxAttempts)
	}
}

// A job that dies in a way the handler cannot turn into a verdict (here: always crashing)
// is dead-lettered by the queue, which knows nothing about the expense. The sweep is what
// gets the expense out of `parsing`.
func TestChain_ADeadJobIsCleanedUpBySweepNotLeftParsing(t *testing.T) {
	r := chainRig(t)
	u := r.uploadSynth(t, 1)
	stopBroken := r.startWorker(t, map[string]worker.Handler{
		receiptparse.KindParse: func(context.Context, queue.Claimed) (worker.Commit, error) { return nil, errors.New("handler bug") },
	})
	r.waitJob(t, u.ReceiptID, "dead")
	stopBroken()
	if e := r.expense(t, u.ExpenseID); e.Status != "parsing" {
		t.Fatalf("status %s: the queue should not have touched the expense", e.Status)
	}
	// Too young to be swept yet.
	r.sweep(t, receiptparse.SweepConfig{StuckAfter: time.Hour})
	if r.expense(t, u.ExpenseID).Status != "parsing" {
		t.Fatal("a young receipt was swept")
	}
	r.ageReceipt(t, u.ReceiptID)
	r.sweep(t, receiptparse.SweepConfig{StuckAfter: time.Minute})
	if !r.abandoned(t, u) {
		t.Fatal("the dead job's expense was not swept")
	}

	// An operator requeues the dead job, and a healthy worker runs it: the expense is no
	// longer `parsing`, so the run changes nothing and does not call the services.
	r.s.Exec(t, `UPDATE jobs SET state = 'queued', attempts = 0, finished_at = NULL, run_at = now() WHERE kind = 'receipt.parse' AND payload->>'receipt_id' = $1`, u.ReceiptID.String())
	r.ex.answer = synth.Generate(1).Answer()
	r.ocr.text = synth.Generate(1).Text()
	calls := r.ex.calls
	r.startWorker(t, map[string]worker.Handler{receiptparse.KindParse: r.handler(receiptparse.Config{})})
	r.waitJob(t, u.ReceiptID, "succeeded")
	if r.ex.calls != calls {
		t.Errorf("the requeued job called the extractor %d times for an expense that was no longer parsing", r.ex.calls-calls)
	}
	if e := r.expense(t, u.ExpenseID); e.Status != "needs_review" || e.Items != 0 || e.Version != 2 {
		t.Errorf("a requeued job changed an expense that was already swept: %+v", e)
	}
}
