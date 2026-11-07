package worker_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/testutil"
	"github.com/riyne/fifty-fifty/internal/worker"
)

func scrape(t *testing.T, srv *httptest.Server) (body string, resp *http.Response) {
	t.Helper()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), resp
}

// sample returns the value of the exposition line that starts with series (a metric
// name with its labels, exactly as written), and whether it exists.
func sample(body, series string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, series+" "); ok {
			v, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}

func wantSamples(t *testing.T, body string, want map[string]float64) {
	t.Helper()
	for series, w := range want {
		if got, ok := sample(body, series); !ok || got != w {
			t.Errorf("%s = %v (present: %v), want %v", series, got, ok, w)
		}
	}
}

func TestMetrics_ExposesTheQueueStateOfEachKind(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind, quiet := newKind("m-state"), newKind("m-quiet")
	enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: time.Now().Add(-90 * time.Second)})
	enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: time.Now().Add(time.Hour)})
	enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1})
	got, err := queue.ClaimJobs(ctx, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: 1, Lease: time.Minute})
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	// The claim took the oldest due job (the 90 s one); the other due job stays ready.

	srv := httptest.NewServer(worker.NewMetrics().Handler(pool, []string{kind, quiet}))
	defer srv.Close()
	body, resp := scrape(t, srv)

	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	wantSamples(t, body, map[string]float64{
		`fifty_queue_jobs{kind="` + kind + `",state="ready"}`:   1,
		`fifty_queue_jobs{kind="` + kind + `",state="delayed"}`: 1,
		`fifty_queue_jobs{kind="` + kind + `",state="running"}`: 1,
		`fifty_queue_jobs{kind="` + kind + `",state="expired"}`: 0,
		`fifty_queue_jobs{kind="` + kind + `",state="dead"}`:    0,
		// a kind with nothing queued still reports, as zeros
		`fifty_queue_jobs{kind="` + quiet + `",state="ready"}`:                         0,
		`fifty_queue_oldest_ready_age_seconds{kind="` + quiet + `"}`:                   0,
		`fifty_worker_jobs_finished_total{kind="` + quiet + `",outcome="failed_dead"}`: 0,
		`fifty_worker_job_attempts_count{kind="` + quiet + `"}`:                        0,
		`fifty_worker_job_attempts_bucket{kind="` + quiet + `",le="+Inf"}`:             0,
	})
	if age, ok := sample(body, `fifty_queue_oldest_ready_age_seconds{kind="`+kind+`"}`); !ok || age < 0 || age > 60 {
		// The remaining ready job was enqueued just now, not 90 s ago.
		t.Errorf("oldest ready age = %v (present %v), want a few seconds at most", age, ok)
	}
	for _, line := range []string{"# TYPE fifty_queue_jobs gauge", "# TYPE fifty_worker_jobs_finished_total counter", "# TYPE fifty_worker_job_attempts histogram"} {
		if !strings.Contains(body, line) {
			t.Errorf("missing %q", line)
		}
	}
}

func TestMetrics_HTTPMethodsAndErrors(t *testing.T) {
	pool := testutil.WorkerPool(t)
	srv := httptest.NewServer(worker.NewMetrics().Handler(pool, nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: %d, Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	resp, err = http.Head(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD: %d", resp.StatusCode)
	}

	// With the database unreachable the scrape fails with a generic 500 and does not
	// leak the error.
	broken, err := pgxpool.New(ctx, "postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer broken.Close()
	bad := httptest.NewServer(worker.NewMetrics().Handler(broken, nil))
	defer bad.Close()
	body, resp := scrape(t, bad)
	if resp.StatusCode != http.StatusInternalServerError || strings.TrimSpace(body) != "queue statistics unavailable" {
		t.Errorf("unreachable database: %d %q", resp.StatusCode, body)
	}
}

// A worker run with Metrics: one job succeeds first time, one fails once then
// succeeds, one fails every time and dies on its third attempt.
func TestMetrics_CountsOutcomesAndTheAttemptsHistogram(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("m-count")
	m := worker.NewMetrics()
	var flakyCalls atomic.Int32
	cfg := fastConfig(pool, kind, func(_ context.Context, j queue.Claimed) (worker.Commit, error) {
		switch string(j.Payload) {
		case `{"mode": "flaky"}`:
			if flakyCalls.Add(1) == 1 {
				return nil, errors.New("transient")
			}
		case `{"mode": "bad"}`:
			return nil, errors.New("always")
		}
		return nil, nil
	})
	cfg.Metrics = m
	start(t, cfg)
	srv := httptest.NewServer(m.Handler(pool, []string{kind}))
	defer srv.Close()

	for _, mode := range []string{"ok", "flaky", "bad"} {
		enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3, Payload: map[string]any{"mode": mode}})
	}
	k := `kind="` + kind + `"`
	waitFor(t, "all three jobs to finish and be counted", 10*time.Second, func() bool {
		body, _ := scrape(t, srv)
		v, _ := sample(body, `fifty_worker_job_attempts_count{`+k+`}`)
		return v == 3
	})
	body, _ := scrape(t, srv)
	wantSamples(t, body, map[string]float64{
		`fifty_worker_jobs_finished_total{` + k + `,outcome="succeeded"}`:    2, // ok, and flaky's second attempt
		`fifty_worker_jobs_finished_total{` + k + `,outcome="failed_retry"}`: 3, // flaky once, bad twice
		`fifty_worker_jobs_finished_total{` + k + `,outcome="failed_dead"}`:  1,
		`fifty_worker_jobs_finished_total{` + k + `,outcome="lease_lost"}`:   0,
		// Final attempts: 1 (ok), 2 (flaky), 3 (bad, dead).
		`fifty_worker_job_attempts_bucket{` + k + `,le="1"}`:    1,
		`fifty_worker_job_attempts_bucket{` + k + `,le="2"}`:    2,
		`fifty_worker_job_attempts_bucket{` + k + `,le="3"}`:    3,
		`fifty_worker_job_attempts_bucket{` + k + `,le="10"}`:   3,
		`fifty_worker_job_attempts_bucket{` + k + `,le="+Inf"}`: 3,
		`fifty_worker_job_attempts_sum{` + k + `}`:              6,
		`fifty_worker_job_attempts_count{` + k + `}`:            3,
		// and the database side agrees: the dead job is in the dead gauge
		`fifty_queue_jobs{` + k + `,state="dead"}`: 1,
	})
}

// A worker that loses a lease counts it, and it does not count as a final attempt.
func TestMetrics_CountsLostLeases(t *testing.T) {
	pool := testutil.WorkerPool(t)
	kind := newKind("m-lost")
	m := worker.NewMetrics()
	started := make(chan struct{})
	cfg := fastConfig(pool, kind, func(ctx context.Context, _ queue.Claimed) (worker.Commit, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	cfg.Lease = 3 * time.Second
	cfg.Metrics = m
	start(t, cfg)
	srv := httptest.NewServer(m.Handler(pool, []string{kind}))
	defer srv.Close()

	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})
	<-started
	waitFor(t, "another worker to take the job over", 5*time.Second, func() bool {
		if _, err := pool.Exec(ctx, `UPDATE jobs SET run_at = now() - interval '1 second', lease_expires_at = now() - interval '1 second' WHERE id = $1 AND state = 'running'`, id); err != nil {
			t.Fatal(err)
		}
		got, err := queue.ClaimJobs(ctx, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: 1, Lease: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return len(got) == 1
	})
	k := `kind="` + kind + `"`
	waitFor(t, "the lost lease to be counted", 10*time.Second, func() bool {
		body, _ := scrape(t, srv)
		v, _ := sample(body, `fifty_worker_jobs_finished_total{`+k+`,outcome="lease_lost"}`)
		return v == 1
	})
	body, _ := scrape(t, srv)
	wantSamples(t, body, map[string]float64{
		`fifty_worker_job_attempts_count{` + k + `}`:                      0,
		`fifty_worker_jobs_finished_total{` + k + `,outcome="succeeded"}`: 0,
	})
}
