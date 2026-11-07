package receiptparse_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/anthropic"
	"github.com/riyne/fifty-fifty/internal/mockllm"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
	"github.com/riyne/fifty-fifty/internal/worker"
)

// withClient points the rig's extractor at url through the real Anthropic client.
func (r *rig) withClient(t *testing.T, url string) receiptparse.Extractor {
	t.Helper()
	c, err := anthropic.New(anthropic.Config{BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (r *rig) handlerWith(ex receiptparse.Extractor) worker.Handler {
	return receiptparse.Handler(r.worker, r.store, r.ocr, ex, receiptparse.Config{ExtractTimeout: 10 * time.Second})
}

// allowAttempts raises the attempts of the receipts' jobs, so that a test with a high fault
// rate does not depend on a receipt surviving four draws in a row.
func (r *rig) allowAttempts(t *testing.T, n int) {
	t.Helper()
	r.s.Exec(t, `UPDATE jobs SET max_attempts = $1 WHERE kind = 'receipt.parse'`, n)
}

// With the real client talking to the mock LLM over HTTP, injected rate limits, server
// errors and cut-off responses are retried by the worker, and every receipt still ends
// with exactly its ground-truth items, written once. (mock LLM with simulated latency of
// 20 ms; the rates are per request, so retries draw again.)
func TestFaults_EveryReceiptSurvivesAFlakyModel(t *testing.T) {
	const n = 14
	r := chainRig(t)
	ups := map[uuid.UUID]int{}
	exp := map[uuid.UUID]upload{}
	for ref := 1; ref <= n; ref++ {
		u := r.uploadSynth(t, ref)
		ups[u.ReceiptID], exp[u.ReceiptID] = ref, u
	}
	r.answersByRef(t, ups)
	r.allowAttempts(t, 12)

	mock, err := mockllm.New(mockllm.Config{
		LatencyMedian: 20 * time.Millisecond, Seed: 7,
		Rate429: 0.15, Rate5xx: 0.15, RateMalformed: 0.10, RetryAfter: time.Second,
		Fixtures: synth.Fixtures(1, n),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mock.Handler())
	defer ts.Close()
	r.startWorker(t, map[string]worker.Handler{receiptparse.KindParse: r.handlerWith(r.withClient(t, ts.URL))})

	for id, ref := range ups {
		j := r.waitJob(t, id, "succeeded")
		truth := synth.Generate(ref)
		units := 0
		for _, l := range truth.Lines {
			units += l.Quantity
		}
		e := r.expense(t, exp[id].ExpenseID)
		if e.Status != "claiming" || e.Items != units || e.ItemSum+e.Tax+e.Tip != truth.TotalMinor || e.Version != 2 || r.audits(t, exp[id].ExpenseID) != 1 {
			t.Errorf("ref %d (job %+v): expense %+v, want %d units totalling %d, version 2, one audit event", ref, j, e, units, truth.TotalMinor)
		}
	}
	st := mock.Stats()
	t.Logf("mock stats: %+v", st)
	if st.Requests <= n || st.RateLimited+st.ServerError+st.Malformed == 0 {
		t.Errorf("the run saw no faults (%+v): it proved nothing about retries", st)
	}
	if st.FromFixture < n {
		t.Errorf("%d fixture answers served for %d receipts", st.FromFixture, n)
	}
}

// retryServer answers the first request with a 429 (with or without a retry-after), then
// serves the mock. It records when each request arrived.
type retryServer struct {
	mu    sync.Mutex
	times []time.Time
	after string // the Retry-After header of the first answer; "" for none
	next  http.Handler
}

func (s *retryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.times = append(s.times, time.Now())
	first := len(s.times) == 1
	s.mu.Unlock()
	if first {
		if s.after != "" {
			w.Header().Set("Retry-After", s.after)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
		return
	}
	s.next.ServeHTTP(w, r)
}

func (s *retryServer) gap(t *testing.T) time.Duration {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.times) < 2 {
		t.Fatalf("%d requests", len(s.times))
	}
	return s.times[1].Sub(s.times[0])
}

// The 429's retry-after reaches the queue: the retry comes after the time the service
// asked for even though the worker's own backoff here is a millisecond, and without the
// header the retry is immediate (so the delay in the first case is the header's doing).
func TestFaults_RetryAfterIsHonoredEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		min, max     time.Duration
	}{
		{"with retry-after: 2", "2", 1900 * time.Millisecond, 6 * time.Second},
		{"without retry-after", "", 0, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chainRig(t)
			u := r.uploadSynth(t, 1)
			r.answersByRef(t, map[uuid.UUID]int{u.ReceiptID: 1})
			mock, _ := mockllm.New(mockllm.Config{Fixtures: synth.Fixtures(1, 1)})
			srv := &retryServer{after: tc.header, next: mock.Handler()}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			r.startWorker(t, map[string]worker.Handler{receiptparse.KindParse: r.handlerWith(r.withClient(t, ts.URL))})

			j := r.waitJob(t, u.ReceiptID, "succeeded")
			if gap := srv.gap(t); gap < tc.min || gap > tc.max {
				t.Errorf("the retry came %v after the 429, want between %v and %v", gap, tc.min, tc.max)
			}
			if e := r.expense(t, u.ExpenseID); e.Status != "claiming" || j.attempts != 2 {
				t.Errorf("expense %+v, job %+v", e, j)
			}
		})
	}
}

func TestFaults_ARetryAfterBeyondTheCapIsCut(t *testing.T) {
	r := chainRig(t)
	u := r.uploadSynth(t, 1)
	r.answersByRef(t, map[uuid.UUID]int{u.ReceiptID: 1})
	mock, _ := mockllm.New(mockllm.Config{Fixtures: synth.Fixtures(1, 1)})
	srv := &retryServer{after: strconv.Itoa(86400), next: mock.Handler()}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	// A worker whose cap is one second: a service asking for a day gets a second.
	r.startWorkerCfg(t, map[string]worker.Handler{receiptparse.KindParse: r.handlerWith(r.withClient(t, ts.URL))}, func(c *worker.Config) { c.MaxRetryAfter = time.Second })
	r.waitJob(t, u.ReceiptID, "succeeded")
	if gap := srv.gap(t); gap < 900*time.Millisecond || gap > 4*time.Second {
		t.Errorf("the retry came %v after a 24 h retry-after with a 1 s cap", gap)
	}
}
