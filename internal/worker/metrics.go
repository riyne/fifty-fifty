package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/queue"
)

// What happened to a job's attempt, as counted in fifty_worker_jobs_finished_total.
const (
	OutcomeSucceeded = "succeeded"    // the job is done
	OutcomeRetry     = "failed_retry" // the attempt failed; the job will run again
	OutcomeDead      = "failed_dead"  // the attempt failed and was the last; the job is dead-lettered
	OutcomeLeaseLost = "lease_lost"   // the lease was taken over; another worker's attempt counts
)

var outcomes = []string{OutcomeSucceeded, OutcomeRetry, OutcomeDead, OutcomeLeaseLost}

// attemptBuckets are the upper bounds of the attempts histogram: how many
// attempts jobs needed to reach a final state.
var attemptBuckets = []float64{1, 2, 3, 4, 5, 10}

// Metrics is what one worker process counts, served together with the queue's
// state from the database as Prometheus text. The counters are per process and
// reset on restart (the usual counter contract: Prometheus sums and rates them);
// the queue gauges come from the database at scrape time, so every worker reports
// the same ones.
//
// The methods are safe for concurrent use and a nil *Metrics ignores everything,
// so a worker without a metrics endpoint pays nothing.
type Metrics struct {
	mu       sync.Mutex
	finished map[finishedKey]uint64
	attempts map[string]*attemptHist
}

type finishedKey struct{ kind, outcome string }

// attemptHist is a Prometheus histogram: cumulative bucket counts, sum and count.
type attemptHist struct {
	buckets []uint64 // buckets[i] counts observations <= attemptBuckets[i]
	sum     float64
	count   uint64
}

// NewMetrics returns an empty Metrics.
func NewMetrics() *Metrics {
	return &Metrics{finished: map[finishedKey]uint64{}, attempts: map[string]*attemptHist{}}
}

// finish records the end of an attempt. Only the attempt that decides a job's fate
// (succeeded, or failed with none left) goes into the attempts histogram: it is
// about how many tries jobs take to finish, and counting each retry as well would
// count a job several times.
func (m *Metrics) finish(kind, outcome string, attempt int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finished[finishedKey{kind, outcome}]++
	if outcome != OutcomeSucceeded && outcome != OutcomeDead {
		return
	}
	h := m.attempts[kind]
	if h == nil {
		h = &attemptHist{buckets: make([]uint64, len(attemptBuckets))}
		m.attempts[kind] = h
	}
	for i, ub := range attemptBuckets {
		if float64(attempt) <= ub {
			h.buckets[i]++
		}
	}
	h.sum += float64(attempt)
	h.count++
}

// Handler serves the metrics in Prometheus text format (version 0.0.4). kinds are
// the job kinds this worker handles; they always get a series, even when idle.
//
// The endpoint has no authentication and reports job counts by kind and nothing
// else (no payloads, ids or errors). Bind it to a private interface.
func (m *Metrics) Handler(pool *pgxpool.Pool, kinds []string) http.Handler {
	kinds = append([]string(nil), kinds...)
	sort.Strings(kinds)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		stats, err := queue.Stats(ctx, pool, kinds)
		if err != nil {
			slog.Error("metrics scrape failed", "err", err)
			http.Error(w, "queue statistics unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.write(w, stats, kinds) // net/http drops the body of a HEAD response itself
	})
}

// write renders the queue statistics and this process's counters.
func (m *Metrics) write(w io.Writer, stats []queue.KindStats, kinds []string) {
	header := func(name, help, typ string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	header("fifty_queue_jobs", "Unfinished jobs by kind and state (ready: queued and due; delayed: queued for later; running: live lease; expired: lease run out, awaiting takeover or reaping; dead: dead-lettered).", "gauge")
	for _, s := range stats {
		for _, st := range []struct {
			state string
			n     int64
		}{{"ready", s.Ready}, {"delayed", s.Delayed}, {"running", s.Running}, {"expired", s.Expired}, {"dead", s.Dead}} {
			fmt.Fprintf(w, "fifty_queue_jobs{kind=\"%s\",state=\"%s\"} %d\n", label(s.Kind), st.state, st.n)
		}
	}

	header("fifty_queue_oldest_ready_age_seconds", "How long the longest-waiting ready job has been due, per kind; 0 when none is ready.", "gauge")
	for _, s := range stats {
		fmt.Fprintf(w, "fifty_queue_oldest_ready_age_seconds{kind=\"%s\"} %g\n", label(s.Kind), s.OldestReadyAge.Seconds())
	}

	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Every handled kind reports every outcome, even at zero, plus any kind seen.
	all := map[string]bool{}
	for _, k := range kinds {
		all[k] = true
	}
	for k := range m.attempts {
		all[k] = true
	}
	for k := range m.finished {
		all[k.kind] = true
	}
	sorted := make([]string, 0, len(all))
	for k := range all {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	header("fifty_worker_jobs_finished_total", "Job attempts finished by this worker process, by kind and outcome.", "counter")
	for _, k := range sorted {
		for _, o := range outcomes {
			fmt.Fprintf(w, "fifty_worker_jobs_finished_total{kind=\"%s\",outcome=\"%s\"} %d\n", label(k), o, m.finished[finishedKey{k, o}])
		}
	}

	header("fifty_worker_job_attempts", "Attempts a job needed to reach a final state (succeeded or dead), by kind, as counted by this worker process.", "histogram")
	for _, k := range sorted {
		h := m.attempts[k]
		if h == nil {
			h = &attemptHist{buckets: make([]uint64, len(attemptBuckets))}
		}
		for i, ub := range attemptBuckets {
			fmt.Fprintf(w, "fifty_worker_job_attempts_bucket{kind=\"%s\",le=\"%g\"} %d\n", label(k), ub, h.buckets[i])
		}
		fmt.Fprintf(w, "fifty_worker_job_attempts_bucket{kind=\"%s\",le=\"+Inf\"} %d\n", label(k), h.count)
		fmt.Fprintf(w, "fifty_worker_job_attempts_sum{kind=\"%s\"} %g\n", label(k), h.sum)
		fmt.Fprintf(w, "fifty_worker_job_attempts_count{kind=\"%s\"} %d\n", label(k), h.count)
	}
}

// label escapes a label value for the text format: backslash, double quote and
// newline. Callers put the result between literal double quotes ("%s"); %q would
// apply Go's escaping on top and double every backslash.
func label(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}
