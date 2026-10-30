package worker

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/queue"
)

// Label values are escaped for the text format: backslash, quote and newline, once
// (a second round of escaping would turn each backslash into two).
func TestWrite_EscapesLabelValues(t *testing.T) {
	var b bytes.Buffer
	(*Metrics)(nil).write(&b, []queue.KindStats{{Kind: "a\"b\\c\nd", Ready: 1, OldestReadyAge: 1500 * time.Millisecond}}, nil)
	for _, want := range []string{
		`fifty_queue_jobs{kind="a\"b\\c\nd",state="ready"} 1`,
		`fifty_queue_oldest_ready_age_seconds{kind="a\"b\\c\nd"} 1.5`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing line %s in:\n%s", want, b.String())
		}
	}
	if strings.Count(b.String(), "\n") != strings.Count(strings.ReplaceAll(b.String(), `\n`, ""), "\n") {
		t.Error("an unescaped newline reached the output inside a label")
	}
}

func TestMetrics_HistogramIsCumulativeAndFinalAttemptsOnly(t *testing.T) {
	m := NewMetrics()
	for _, a := range []int{1, 1, 2, 4, 7, 12} {
		m.finish("k", OutcomeSucceeded, a)
	}
	m.finish("k", OutcomeDead, 3)
	m.finish("k", OutcomeRetry, 1)     // a retry is not a final attempt
	m.finish("k", OutcomeLeaseLost, 2) // neither is a lost lease

	var b bytes.Buffer
	m.write(&b, nil, []string{"k"})
	out := b.String()
	for _, want := range []string{
		`fifty_worker_job_attempts_bucket{kind="k",le="1"} 2`,
		`fifty_worker_job_attempts_bucket{kind="k",le="2"} 3`,
		`fifty_worker_job_attempts_bucket{kind="k",le="3"} 4`,
		`fifty_worker_job_attempts_bucket{kind="k",le="4"} 5`,
		`fifty_worker_job_attempts_bucket{kind="k",le="5"} 5`,
		`fifty_worker_job_attempts_bucket{kind="k",le="10"} 6`,
		`fifty_worker_job_attempts_bucket{kind="k",le="+Inf"} 7`,
		`fifty_worker_job_attempts_sum{kind="k"} 30`,
		`fifty_worker_job_attempts_count{kind="k"} 7`,
		`fifty_worker_jobs_finished_total{kind="k",outcome="succeeded"} 6`,
		`fifty_worker_jobs_finished_total{kind="k",outcome="failed_dead"} 1`,
		`fifty_worker_jobs_finished_total{kind="k",outcome="failed_retry"} 1`,
		`fifty_worker_jobs_finished_total{kind="k",outcome="lease_lost"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing line %q in:\n%s", want, out)
		}
	}
}

func TestMetrics_NilIsHarmless(t *testing.T) {
	var m *Metrics
	m.finish("k", OutcomeSucceeded, 1) // must not panic
	var b bytes.Buffer
	m.write(&b, []queue.KindStats{{Kind: "k"}}, []string{"k"})
	if !strings.Contains(b.String(), `fifty_queue_jobs{kind="k",state="ready"} 0`) {
		t.Errorf("nil Metrics should still report the queue:\n%s", b.String())
	}
	if strings.Contains(b.String(), "fifty_worker_") {
		t.Errorf("nil Metrics reported worker counters:\n%s", b.String())
	}
}
