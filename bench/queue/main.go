// Command queue runs the queue benchmarks:
// throughput against worker count, end-to-end and queue-wait latency under steady
// load, and retry behavior under injected faults. Every scenario drives real
// cmd/benchworker processes through Postgres against the mock LLM (cmd/mockllm's
// server, in-process here); every result says "mock LLM with simulated latency of
// X ms".
//
//	go run ./bench/queue                      # everything, 3 runs each
//	go run ./bench/queue --scenarios latency --runs 5
//
// It writes <out>/queue-<timestamp>.json (every run, parameters, environment) and a
// .txt summary, and refuses a dirty working tree unless --allow-dirty (smoke runs;
// do not commit those results). See README.md in this directory for what each scenario means.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/riyne/fifty-fifty/internal/harness/benchwork"
	"github.com/riyne/fifty-fifty/internal/harness/mockllm"
)

const (
	concurrency = 4 // jobs in flight per worker, in every scenario
	lease       = 30 * time.Second
	poll        = time.Second // the worker default
)

type options struct {
	Runs           int      `json:"runs_per_configuration"`
	Scenarios      []string `json:"scenarios"`
	Workers        []int    `json:"worker_counts"`
	JobsPerSlot    int      `json:"throughput_jobs_per_slot"`
	QueueBoundJobs int      `json:"queue_bound_jobs"`
	LatencyMedian  string   `json:"llm_latency_median"`
	LatencySigma   float64  `json:"llm_latency_sigma"`
	Utilization    float64  `json:"latency_scenario_offered_load_fraction_of_capacity"`
	LatencyRun     string   `json:"latency_scenario_arrival_duration"`
	LowRate        float64  `json:"latency_low_scenario_offered_jobs_per_second"`
	LowWorkers     []int    `json:"latency_low_scenario_worker_counts"`
	LowRun         string   `json:"latency_low_scenario_arrival_duration"`
	RetryJobs      int      `json:"retry_scenario_jobs"`
	RetryWorkers   int      `json:"retry_scenario_workers"`
	Concurrency    int      `json:"concurrency_per_worker"`
	Poll           string   `json:"poll_interval"`
	Lease          string   `json:"lease"`
	MaxAttempts    int      `json:"max_attempts"`
}

// configRun is one configuration (a scenario at one worker count or fault mix) and
// its repeated runs.
type configRun struct {
	Scenario    string                 `json:"scenario"`
	Label       string                 `json:"label"`
	Workers     int                    `json:"workers"`
	LLMLatency  string                 `json:"mock_llm_simulated_latency"`
	Faults      map[string]float64     `json:"mock_llm_fault_rates,omitempty"`
	OfferedRate float64                `json:"offered_jobs_per_second,omitempty"`
	TheoryRate  float64                `json:"theoretical_max_jobs_per_second,omitempty"`
	Runs        []benchwork.LoadResult `json:"runs"`
}

type report struct {
	StartedAt time.Time     `json:"started_at"`
	Options   options       `json:"options"`
	Env       benchwork.Env `json:"environment"`
	Configs   []configRun   `json:"results"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "queue bench:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		o          options
		scen, wk   string
		median     time.Duration
		latencyDur time.Duration
		lowDur     time.Duration
		out        string
		allowDirty bool
	)
	flag.IntVar(&o.Runs, "runs", 3, "runs per configuration")
	flag.StringVar(&scen, "scenarios", "all", "comma-separated: throughput-llm, throughput-queue, latency, latency-low, retry (or all)")
	flag.StringVar(&wk, "workers", "1,2,4,8", "worker process counts")
	flag.IntVar(&o.JobsPerSlot, "jobs-per-slot", 40, "throughput-llm: jobs per in-flight slot")
	flag.IntVar(&o.QueueBoundJobs, "queue-bound-jobs", 10000, "throughput-queue: jobs per run")
	flag.DurationVar(&median, "latency-median", 800*time.Millisecond, "mock LLM median latency (throughput-llm, latency, retry)")
	flag.Float64Var(&o.LatencySigma, "latency-sigma", 0.4, "mock LLM latency sigma")
	flag.Float64Var(&o.Utilization, "load-fraction", 0.5, "latency: offered load as a fraction of theoretical capacity")
	flag.DurationVar(&latencyDur, "latency-duration", 30*time.Second, "latency: how long jobs arrive")
	flag.Float64Var(&o.LowRate, "low-rate", 1, "latency-low: offered jobs per second (workers are mostly idle)")
	flag.DurationVar(&lowDur, "low-duration", 40*time.Second, "latency-low: how long jobs arrive")
	flag.IntVar(&o.RetryJobs, "retry-jobs", 500, "retry: jobs per run")
	flag.IntVar(&o.RetryWorkers, "retry-workers", 4, "retry: worker processes")
	flag.StringVar(&out, "out", "bench/results", "directory for the result files")
	flag.BoolVar(&allowDirty, "allow-dirty", false, "run on a working tree with uncommitted changes (smoke runs only)")
	flag.Parse()

	ownerURL, appURL := os.Getenv("MIGRATE_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		return errors.New("MIGRATE_DATABASE_URL (owner) and DATABASE_URL (app role) are required; the Makefile exports both")
	}
	for _, f := range strings.Split(wk, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			return fmt.Errorf("bad worker count %q", f)
		}
		o.Workers = append(o.Workers, n)
	}
	all := []string{"throughput-llm", "throughput-queue", "latency", "latency-low", "retry"}
	o.Scenarios = all
	if scen != "all" {
		o.Scenarios = nil
		for _, s := range strings.Split(scen, ",") {
			s = strings.TrimSpace(s)
			if !contains(all, s) {
				return fmt.Errorf("unknown scenario %q", s)
			}
			o.Scenarios = append(o.Scenarios, s)
		}
	}
	o.Concurrency, o.Poll, o.Lease, o.MaxAttempts = concurrency, poll.String(), lease.String(), 5
	o.LatencyMedian, o.LatencyRun, o.LowRun, o.LowWorkers = median.String(), latencyDur.String(), lowDur.String(), []int{1, 4}

	ctx := context.Background()
	env, err := benchwork.CaptureEnv(ctx, ownerURL)
	if err != nil {
		return err
	}
	if env.Dirty && !allowDirty {
		return errors.New("working tree has uncommitted changes; commit first so the recorded commit hash is the code that ran (or --allow-dirty for a smoke run)")
	}
	tmp, err := os.MkdirTemp("", "queue-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "benchworker")
	if b, err := exec.Command("go", "build", "-o", bin, "./cmd/benchworker").CombinedOutput(); err != nil {
		return fmt.Errorf("build benchworker: %v\n%s", err, b)
	}

	base := benchwork.LoadParams{
		DatabaseURL: appURL, OwnerURL: ownerURL, WorkerBin: bin,
		Concurrency: concurrency, Lease: lease, Poll: poll, MaxAttempts: o.MaxAttempts,
		DrainTimeout: 10 * time.Minute,
	}
	// mean of a log-normal: median * exp(sigma^2 / 2)
	meanLatency := time.Duration(float64(median) * math.Exp(o.LatencySigma*o.LatencySigma/2))

	rep := report{StartedAt: time.Now().UTC(), Options: o, Env: env}
	exec1 := func(c configRun, mk func(seed uint64) benchwork.LoadParams) error {
		for i := 1; i <= o.Runs; i++ {
			fmt.Fprintf(os.Stderr, "%s %s: run %d/%d\n", c.Scenario, c.Label, i, o.Runs)
			p := mk(uint64(i))
			p.Seed = uint64(i)
			res, err := benchwork.RunLoad(ctx, p)
			if err != nil {
				return fmt.Errorf("%s %s run %d: %w", c.Scenario, c.Label, i, err)
			}
			c.Runs = append(c.Runs, res)
		}
		rep.Configs = append(rep.Configs, c)
		return nil
	}

	for _, s := range o.Scenarios {
		switch s {
		case "throughput-llm":
			for _, w := range o.Workers {
				w := w
				c := configRun{Scenario: s, Label: fmt.Sprintf("%d workers", w), Workers: w, LLMLatency: median.String(),
					TheoryRate: float64(w*concurrency) / meanLatency.Seconds()}
				err = exec1(c, func(seed uint64) benchwork.LoadParams {
					p := base
					p.Workers, p.Jobs, p.Lean = w, o.JobsPerSlot*w*concurrency, true
					p.Mock = mockllm.Config{LatencyMedian: median, LatencySigma: o.LatencySigma, Seed: seed}
					return p
				})
				if err != nil {
					break
				}
			}
		case "throughput-queue":
			for _, w := range o.Workers {
				w := w
				c := configRun{Scenario: s, Label: fmt.Sprintf("%d workers", w), Workers: w, LLMLatency: "0s"}
				err = exec1(c, func(seed uint64) benchwork.LoadParams {
					p := base
					p.Workers, p.Jobs, p.Lean = w, o.QueueBoundJobs, true
					p.Mock = mockllm.Config{Seed: seed}
					return p
				})
				if err != nil {
					break
				}
			}
		case "latency":
			for _, w := range o.Workers {
				w := w
				theory := float64(w*concurrency) / meanLatency.Seconds()
				c := configRun{Scenario: s, Label: fmt.Sprintf("%d workers", w), Workers: w, LLMLatency: median.String(),
					TheoryRate: theory, OfferedRate: theory * o.Utilization}
				err = exec1(c, func(seed uint64) benchwork.LoadParams {
					p := base
					p.Workers, p.Rate, p.Duration = w, c.OfferedRate, latencyDur
					p.Mock = mockllm.Config{LatencyMedian: median, LatencySigma: o.LatencySigma, Seed: seed}
					return p
				})
				if err != nil {
					break
				}
			}
		case "latency-low":
			for _, w := range o.LowWorkers {
				w := w
				c := configRun{Scenario: s, Label: fmt.Sprintf("%d workers", w), Workers: w, LLMLatency: median.String(),
					TheoryRate: float64(w*concurrency) / meanLatency.Seconds(), OfferedRate: o.LowRate}
				err = exec1(c, func(seed uint64) benchwork.LoadParams {
					p := base
					p.Workers, p.Rate, p.Duration = w, o.LowRate, lowDur
					p.Mock = mockllm.Config{LatencyMedian: median, LatencySigma: o.LatencySigma, Seed: seed}
					return p
				})
				if err != nil {
					break
				}
			}
		case "retry":
			for _, f := range []struct {
				name           string
				r429, r5, rbad float64
			}{{"no faults", 0, 0, 0}, {"moderate", 0.10, 0.05, 0.05}, {"heavy", 0.20, 0.15, 0.10}} {
				f := f
				c := configRun{Scenario: s, Label: f.name, Workers: o.RetryWorkers, LLMLatency: median.String(),
					Faults: map[string]float64{"429": f.r429, "5xx": f.r5, "malformed_200": f.rbad}}
				err = exec1(c, func(seed uint64) benchwork.LoadParams {
					p := base
					p.Workers, p.Jobs, p.Lean = o.RetryWorkers, o.RetryJobs, true
					p.Mock = mockllm.Config{LatencyMedian: median, LatencySigma: o.LatencySigma, Seed: seed,
						Rate429: f.r429, Rate5xx: f.r5, RateMalformed: f.rbad}
					return p
				})
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			return err
		}
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	name := filepath.Join(out, "queue-"+rep.StartedAt.Format("20060102T150405Z"))
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(name+".json", append(raw, '\n'), 0o644); err != nil {
		return err
	}
	text := render(rep)
	if err := os.WriteFile(name+".txt", []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Print(text)
	fmt.Println("wrote", name+".json and .txt")
	if a := anomalies(rep); a != "" {
		return errors.New("ANOMALIES: " + a)
	}
	return nil
}

// anomalies reports what must never happen in any scenario (a duplicate or missing
// effect, a job that never finished) and, outside the retry scenario where
// dead-letters are expected, a dead-lettered job.
func anomalies(r report) string {
	var out []string
	for _, c := range r.Configs {
		for i, run := range c.Runs {
			switch {
			case run.DuplicateFx != 0 || run.MissingFx != 0:
				out = append(out, fmt.Sprintf("%s/%s run %d: %d duplicate, %d missing effects", c.Scenario, c.Label, i+1, run.DuplicateFx, run.MissingFx))
			case run.Unfinished != 0:
				out = append(out, fmt.Sprintf("%s/%s run %d: %d jobs unfinished", c.Scenario, c.Label, i+1, run.Unfinished))
			case c.Scenario != "retry" && run.Dead != 0:
				out = append(out, fmt.Sprintf("%s/%s run %d: %d dead jobs", c.Scenario, c.Label, i+1, run.Dead))
			case run.Succeeded+run.Dead != run.Enqueued:
				out = append(out, fmt.Sprintf("%s/%s run %d: %d enqueued, %d succeeded, %d dead", c.Scenario, c.Label, i+1, run.Enqueued, run.Succeeded, run.Dead))
			}
		}
	}
	return strings.Join(out, "; ")
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
