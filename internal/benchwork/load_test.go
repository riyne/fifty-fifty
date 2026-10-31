package benchwork_test

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/benchwork"
	"github.com/riyne/fifty-fifty/internal/mockllm"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

func loadParams(t *testing.T) benchwork.LoadParams {
	t.Helper()
	testutil.Pool(t) // skips without TEST_DATABASE_URL
	testutil.OwnerPool(t)
	return benchwork.LoadParams{
		DatabaseURL: os.Getenv("TEST_DATABASE_URL"), OwnerURL: os.Getenv("TEST_OWNER_DATABASE_URL"),
		WorkerBin: buildWorker(t), Workers: 2, Concurrency: 3,
		Lease: 5 * time.Second, Poll: 100 * time.Millisecond,
		Mock: mockllm.Config{LatencyMedian: 30 * time.Millisecond, Seed: 1},
		Logf: t.Logf, DrainTimeout: time.Minute,
	}
}

func runCount(t *testing.T, run string) (n int) {
	t.Helper()
	pool := testutil.Pool(t)
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'run' = $2`, benchwork.Kind, run).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// benchRows counts all rows in the bench tables, to show a run cleans up after itself.
func benchRows(t *testing.T) (n int) {
	t.Helper()
	pool := testutil.Pool(t)
	err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM bench.effects) + (SELECT count(*) FROM bench.attempts)`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunLoad_ClosedLoop(t *testing.T) {
	for _, lean := range []bool{false, true} {
		name := map[bool]string{false: "instrumented", true: "lean"}[lean]
		t.Run(name, func(t *testing.T) {
			p := loadParams(t)
			p.Jobs, p.Lean = 60, lean
			before := benchRows(t)
			res, err := benchwork.RunLoad(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if after := benchRows(t); after != before {
				t.Errorf("bench tables hold %d rows after the run, %d before: the run left rows behind", after, before)
			}
			if wantRows := map[bool]int{false: 60, true: 0}[lean]; res.AttemptRows != wantRows {
				t.Errorf("%d bookkeeping rows, want %d: a lean run must write none and an instrumented run one per attempt", res.AttemptRows, wantRows)
			}
			if math.Abs(res.ThroughputPerSec*res.SpanSecs-float64(res.Succeeded-1)) > 1e-6 {
				t.Errorf("throughput %v x span %v != succeeded-1 = %d", res.ThroughputPerSec, res.SpanSecs, res.Succeeded-1)
			}
			t.Logf("%+v", res)
			if res.Enqueued != 60 || res.Succeeded != 60 || res.Dead != 0 || res.Unfinished != 0 {
				t.Errorf("counts: %+v", res)
			}
			if res.DuplicateFx != 0 || res.MissingFx != 0 {
				t.Errorf("effects: %d duplicates, %d missing", res.DuplicateFx, res.MissingFx)
			}
			// Each attempt makes exactly one call, and nothing was killed, so the mock
			// saw exactly the attempts the queue counted: the two sources agree.
			if res.TotalAttempts != 60 || res.Mock.Requests != 60 || res.AttemptsHist[1] != 60 {
				t.Errorf("attempts %d, mock requests %d, hist %v", res.TotalAttempts, res.Mock.Requests, res.AttemptsHist)
			}
			if res.ThroughputPerSec <= 0 || res.SpanSecs <= 0 || res.EndToEnd.N != 60 || res.EndToEnd.Min < 0.03 {
				t.Errorf("throughput %v over %vs, end-to-end %+v (every job takes at least the 30ms mock latency)",
					res.ThroughputPerSec, res.SpanSecs, res.EndToEnd)
			}
			if wantWait := map[bool]int{false: 60, true: 0}[lean]; res.QueueWait.N != wantWait {
				t.Errorf("queue wait has %d samples, want %d", res.QueueWait.N, wantWait)
			}
			if n := runCount(t, res.Run); n != 0 {
				t.Errorf("%d rows of the run were left behind", n)
			}
		})
	}
}

func TestRunLoad_OpenLoop(t *testing.T) {
	p := loadParams(t)
	p.Rate, p.Duration, p.Warmup, p.Seed = 30, 2*time.Second, time.Second, 7
	res, err := benchwork.RunLoad(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", res)
	// 60 expected arrivals; a Poisson count is within 5 standard deviations (~39) of it.
	if res.Enqueued < 20 || res.Enqueued > 100 {
		t.Errorf("enqueued %d jobs at 30/s for 2s", res.Enqueued)
	}
	if res.Succeeded != res.Enqueued || res.Dead != 0 || res.Unfinished != 0 || res.DuplicateFx != 0 || res.MissingFx != 0 {
		t.Errorf("outcome: %+v", res)
	}
	if res.QueueWait.N != res.Enqueued || res.EndToEnd.N != res.Enqueued {
		t.Errorf("samples: queue wait %d, end to end %d, enqueued %d", res.QueueWait.N, res.EndToEnd.N, res.Enqueued)
	}
	if res.EndToEnd.Median < res.QueueWait.Median+0.03-0.005 {
		t.Errorf("median end-to-end %.3fs is less than median queue wait %.3fs plus the 30ms call", res.EndToEnd.Median, res.QueueWait.Median)
	}
}

// With faults injected, every job ends succeeded or dead after at most MaxAttempts,
// the attempts the queue counted equal the calls the mock saw, and the histogram
// accounts for every job.
func TestRunLoad_FaultsEndInSuccessOrDeadLetter(t *testing.T) {
	p := loadParams(t)
	p.Jobs, p.MaxAttempts = 30, 3
	p.Mock = mockllm.Config{LatencyMedian: 20 * time.Millisecond, Rate429: 0.3, Rate5xx: 0.2, RateMalformed: 0.1, Seed: 5}
	res, err := benchwork.RunLoad(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", res)
	if res.Succeeded+res.Dead != 30 || res.Unfinished != 0 {
		t.Fatalf("succeeded %d + dead %d != 30 (unfinished %d)", res.Succeeded, res.Dead, res.Unfinished)
	}
	if res.Dead == 0 || res.Succeeded == 0 || res.AttemptsHist[2]+res.AttemptsHist[3] == 0 {
		t.Errorf("with a 60%% fault rate and 3 attempts, expected both successes and dead letters and some retries: %+v", res)
	}
	jobs, attempts := 0, 0
	for a, n := range res.AttemptsHist {
		if a < 1 || a > 3 {
			t.Errorf("%d jobs ended with %d attempts; the limit is 3", n, a)
		}
		jobs, attempts = jobs+n, attempts+a*n
	}
	if jobs != 30 || attempts != res.TotalAttempts || int64(res.TotalAttempts) != res.Mock.Requests {
		t.Errorf("histogram covers %d jobs / %d attempts; total attempts %d; mock requests %d", jobs, attempts, res.TotalAttempts, res.Mock.Requests)
	}
	if res.DuplicateFx != 0 || res.MissingFx != 0 {
		t.Errorf("effects: %d duplicates, %d missing", res.DuplicateFx, res.MissingFx)
	}
	if res.Mock.RateLimited == 0 || res.Mock.ServerError == 0 || res.Mock.Malformed == 0 {
		t.Errorf("mock faults seen: %+v", res.Mock)
	}
}

func TestRunLoad_RejectsBadParams(t *testing.T) {
	p := loadParams(t)
	for name, mutate := range map[string]func(*benchwork.LoadParams){
		"neither mode":      func(p *benchwork.LoadParams) {},
		"both modes":        func(p *benchwork.LoadParams) { p.Jobs, p.Rate, p.Duration = 10, 5, time.Second },
		"rate without time": func(p *benchwork.LoadParams) { p.Rate = 5 },
		"no workers":        func(p *benchwork.LoadParams) { p.Jobs, p.Workers = 10, 0 },
	} {
		q := p
		mutate(&q)
		if _, err := benchwork.RunLoad(context.Background(), q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
