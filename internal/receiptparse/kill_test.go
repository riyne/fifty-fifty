package receiptparse_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/anthropic"
	"github.com/riyne/fifty-fifty/internal/harness/mockllm"
	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
	"github.com/riyne/fifty-fifty/internal/worker"
)

// ---- a worker in its own process, so it can be killed ----

// refOCR "reads" the receipt whose stored image it is given, by looking its object key up
// in a map of key to synthetic receipt ref (the images are rendered from known ground
// truth; the real-binary version with real Tesseract is in test/e2e).
type refOCR struct{ refs map[string]int }

func (o refOCR) Text(_ context.Context, img []byte, _ string) (string, error) {
	ref, ok := o.refs[objstore.Key(img)]
	if !ok {
		return "", fmt.Errorf("no ground truth for image %s", objstore.Key(img))
	}
	return synth.Generate(ref).Text(), nil
}

const helperEnv = "RECEIPTPARSE_HELPER_WORKER"

// TestHelperWorker is not a test: it is the body of the worker process the kill test
// starts (the test binary run again with helperEnv set). It runs a real worker with the
// real parse handler and the real Anthropic client, and never returns until it is
// signaled (or killed).
func TestHelperWorker(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("only runs as the kill test's worker process")
	}
	var refs map[string]int
	if err := json.Unmarshal([]byte(os.Getenv("HELPER_REFS")), &refs); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, os.Getenv("HELPER_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store, err := objstore.NewFS(os.Getenv("HELPER_STORE_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := anthropic.New(anthropic.Config{BaseURL: os.Getenv("HELPER_MOCK_URL")})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stop()
	err = worker.Run(runCtx, worker.Config{
		Pool:     pool,
		Handlers: map[string]worker.Handler{receiptparse.KindParse: receiptparse.Handler(pool, store, refOCR{refs}, client, receiptparse.Config{ExtractTimeout: 30 * time.Second})},
		// A short lease so a killed worker's job is taken over within about a second.
		Concurrency: 2, PollInterval: 20 * time.Millisecond, Lease: time.Second, ReapInterval: 200 * time.Millisecond,
		ShutdownGrace: time.Second, Backoff: queue.Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
}

type helper struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (r *rig) startHelper(t *testing.T, dbURL, mockURL string, refs map[string]int) *helper {
	t.Helper()
	refsJSON, _ := json.Marshal(refs)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWorker$", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=1", "HELPER_REFS="+string(refsJSON), "HELPER_DB_URL="+dbURL,
		"HELPER_STORE_DIR="+r.s.ReceiptDir, "HELPER_MOCK_URL="+mockURL)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helper{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(h.done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-h.done })
	return h
}

// A worker killed with SIGKILL in the middle of the model call leaves nothing half done:
// the job's lease runs out, another worker takes it over, and the receipt ends with its
// ground-truth items exactly once. The model call itself is at-least-once (it is made
// again), which the test shows rather than hides. mock LLM with simulated latency of 1500 ms.
func TestKill_AWorkerKilledMidCallLeavesNothingHalfDone(t *testing.T) {
	r := chainRig(t)
	const ref = 7
	u := r.uploadSynth(t, ref)
	var key string
	if err := r.s.Pool.QueryRow(ctx, `SELECT object_key FROM receipts WHERE id = $1`, u.ReceiptID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	refs := map[string]int{key: ref}

	mock, err := mockllm.New(mockllm.Config{LatencyMedian: 1500 * time.Millisecond, Fixtures: synth.Fixtures(ref, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mock.Handler())
	defer ts.Close()
	appURL := scratchWorkerURL(t, r)

	first := r.startHelper(t, appURL, ts.URL, refs)
	// Wait until the first worker is inside the model call: the mock has the request and
	// is holding the answer, and the job is running its first attempt.
	deadline := time.Now().Add(30 * time.Second)
	for mock.Stats().Requests < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the first worker never called the model")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if j := r.parseJob(t, u.ReceiptID); j.state != "running" || j.attempts != 1 {
		t.Fatalf("job %+v, want running on its first attempt", j)
	}
	if err := first.cmd.Process.Kill(); err != nil { // SIGKILL: no cleanup, no goodbye
		t.Fatal(err)
	}
	<-first.done
	if e := r.expense(t, u.ExpenseID); e.Status != "parsing" || e.Items != 0 || e.Version != 1 {
		t.Fatalf("a killed worker left %+v", e)
	}

	// The dead worker's lease (1 s) runs out; a second worker takes the job over.
	r.startHelper(t, appURL, ts.URL, refs)
	j := r.waitJob(t, u.ReceiptID, "succeeded")

	truth := synth.Generate(ref)
	units := 0
	for _, l := range truth.Lines {
		units += l.Quantity
	}
	e := r.expense(t, u.ExpenseID)
	if e.Status != "claiming" || e.Items != units || e.ItemSum+e.Tax+e.Tip != truth.TotalMinor || e.Version != 2 || r.audits(t, u.ExpenseID) != 1 {
		t.Errorf("after the takeover: %+v, want %d units totalling %d at version 2 with one audit event", e, units, truth.TotalMinor)
	}
	if j.attempts != 2 {
		t.Errorf("job attempts %d, want 2 (the killed one and the takeover)", j.attempts)
	}
	if st := mock.Stats(); st.Requests < 2 {
		t.Errorf("model requests %d: the takeover should have called the model again", st.Requests)
	}
}
