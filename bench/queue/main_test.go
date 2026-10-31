package main

import (
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/benchwork"
	"github.com/riyne/fifty-fifty/internal/mockllm"
)

func okRun() benchwork.LoadResult {
	return benchwork.LoadResult{Enqueued: 10, Succeeded: 10, TotalAttempts: 10, AttemptsHist: map[int]int{1: 10}, ThroughputPerSec: 5}
}

func TestAnomalies(t *testing.T) {
	clean := report{Configs: []configRun{{Scenario: "latency", Label: "1 workers", Runs: []benchwork.LoadResult{okRun()}}}}
	if a := anomalies(clean); a != "" {
		t.Fatalf("clean report flagged: %s", a)
	}
	for name, mutate := range map[string]func(*benchwork.LoadResult){
		"duplicate effect":   func(r *benchwork.LoadResult) { r.DuplicateFx = 1 },
		"missing effect":     func(r *benchwork.LoadResult) { r.MissingFx = 1 },
		"unfinished":         func(r *benchwork.LoadResult) { r.Unfinished = 1; r.Succeeded = 9 },
		"dead outside retry": func(r *benchwork.LoadResult) { r.Dead = 1; r.Succeeded = 9 },
		"lost count":         func(r *benchwork.LoadResult) { r.Succeeded = 9 },
	} {
		r := okRun()
		mutate(&r)
		rep := report{Configs: []configRun{{Scenario: "throughput-llm", Label: "x", Runs: []benchwork.LoadResult{r}}}}
		if anomalies(rep) == "" {
			t.Errorf("%s not flagged", name)
		}
	}
	// Dead-letters are expected in the retry scenario, as long as every job is accounted for.
	r := okRun()
	r.Dead, r.Succeeded = 1, 9
	if a := anomalies(report{Configs: []configRun{{Scenario: "retry", Label: "heavy", Runs: []benchwork.LoadResult{r}}}}); a != "" {
		t.Errorf("dead job in the retry scenario flagged: %s", a)
	}
}

func TestRender_EveryScenarioAppearsWithItsLabels(t *testing.T) {
	run := okRun()
	run.EndToEnd = benchwork.Distribution([]float64{1, 2, 3})
	run.QueueWait = benchwork.Distribution([]float64{0.1, 0.2})
	run.Mock = mockllm.Stats{Requests: 10, RateLimited: 1}
	rep := report{
		Options: options{Runs: 1, Scenarios: []string{"throughput-llm", "throughput-queue", "latency", "retry"}, Concurrency: 4, MaxAttempts: 5,
			JobsPerSlot: 40, QueueBoundJobs: 100, LatencyMedian: "800ms", LatencySigma: 0.4, Utilization: 0.5, LatencyRun: "30s", RetryJobs: 10, RetryWorkers: 4},
		Configs: []configRun{
			{Scenario: "throughput-llm", Label: "2 workers", TheoryRate: 10, Runs: []benchwork.LoadResult{run}},
			{Scenario: "throughput-queue", Label: "2 workers", Runs: []benchwork.LoadResult{run}},
			{Scenario: "latency", Label: "2 workers", OfferedRate: 4, Runs: []benchwork.LoadResult{run}},
			{Scenario: "retry", Label: "moderate", Faults: map[string]float64{"429": 0.1, "5xx": 0.05, "malformed_200": 0.05}, Runs: []benchwork.LoadResult{run}},
		},
	}
	text := render(rep)
	for _, want := range []string{"throughput-llm", "throughput-queue", "latency:", "retry:", "mock LLM with simulated latency of", "moderate", "median/theoretical 50%", "jobs by number of attempts: 1: 10, 2: 0"} {
		if !strings.Contains(text, want) {
			t.Errorf("render output lacks %q:\n%s", want, text)
		}
	}
}
