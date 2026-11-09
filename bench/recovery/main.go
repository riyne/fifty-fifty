// Command recovery runs the crash-recovery benchmark:
// kill -9 a worker process mid-job, R times per lease setting, and record how long
// the orphaned jobs take to finish and whether any job was lost or had its effect
// written twice. The workload is internal/benchwork's mock.extract against the mock
// LLM; every result says "mock LLM with simulated latency of X ms".
//
//	go run ./bench/recovery --trials 20 --lease 3s,10s --out bench/results
//
// It writes <out>/recovery-<timestamp>.json (everything, including every trial) and
// a .txt summary. The environment is recorded: machine, Docker limits, Postgres
// version, commit hash and whether the working tree was clean. Results from a dirty
// tree are refused unless --allow-dirty is given (for smoke runs; do not commit
// those).
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
	"strings"
	"time"

	"github.com/riyne/fifty-fifty/internal/benchwork"
	"github.com/riyne/fifty-fifty/internal/mockllm"
)

type params struct {
	Trials      int      `json:"trials_per_lease"`
	Leases      []string `json:"leases"`
	Workers     int      `json:"workers"`
	Concurrency int      `json:"concurrency_per_worker"`
	Jobs        int      `json:"jobs_per_trial"`
	Poll        string   `json:"poll_interval"`
	KillPoint   string   `json:"kill_point"`
	KillJitter  string   `json:"kill_jitter_max"`
	MockMedian  string   `json:"mock_llm_latency_median"`
	MockSigma   float64  `json:"mock_llm_latency_sigma"`
	MaxAttempts int      `json:"max_attempts"`
}

type summary struct {
	Trials         int            `json:"trials"`
	Jobs           int            `json:"jobs_total"`
	Succeeded      int            `json:"succeeded_total"`
	LostJobs       int            `json:"lost_jobs_total"`
	Dead           int            `json:"dead_total"`
	DuplicateFx    int            `json:"duplicate_effects_total"`
	MissingFx      int            `json:"missing_effects_total"`
	Orphans        int            `json:"orphans_total"`
	TrialsNoOrphan int            `json:"trials_with_no_orphan"`
	ExtraAttempts  int            `json:"extra_attempts_total"`
	PerOrphanSecs  benchwork.Dist `json:"recovery_seconds_per_orphan"`
	PerTrialSecs   benchwork.Dist `json:"recovery_seconds_per_trial_all_orphans_done"`
}

type configReport struct {
	Lease   string                     `json:"lease"`
	Trials  []benchwork.RecoveryResult `json:"trials"`
	Summary summary                    `json:"summary"`
}

type report struct {
	StartedAt time.Time      `json:"started_at"`
	Params    params         `json:"params"`
	Env       benchwork.Env  `json:"environment"`
	Configs   []configReport `json:"results"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "recovery:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		p          params
		leasesFlag string
		poll, jit  time.Duration
		median     time.Duration
		out        string
		allowDirty bool
	)
	flag.IntVar(&p.Trials, "trials", 20, "trials per lease setting")
	flag.StringVar(&leasesFlag, "lease", "3s,10s", "comma-separated lease durations to measure")
	flag.IntVar(&p.Workers, "workers", 3, "worker processes (one is killed)")
	flag.IntVar(&p.Concurrency, "concurrency", 4, "jobs in flight per worker")
	flag.IntVar(&p.Jobs, "jobs", 24, "jobs per trial (at least 2 * workers * concurrency)")
	flag.DurationVar(&poll, "poll", time.Second, "worker poll interval")
	flag.DurationVar(&jit, "kill-jitter", time.Second, "kill after a random delay in [0, this) once the victim is busy")
	flag.DurationVar(&median, "latency-median", 800*time.Millisecond, "mock LLM median latency")
	flag.Float64Var(&p.MockSigma, "latency-sigma", 0.4, "mock LLM latency sigma")
	flag.StringVar(&out, "out", "bench/results", "directory for the result files")
	flag.BoolVar(&allowDirty, "allow-dirty", false, "run on a working tree with uncommitted changes (smoke runs only)")
	flag.Parse()

	ownerURL, appURL := os.Getenv("MIGRATE_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		return errors.New("MIGRATE_DATABASE_URL (owner) and DATABASE_URL (app role) are required; the Makefile exports both")
	}
	var leases []time.Duration
	for _, f := range strings.Split(leasesFlag, ",") {
		d, err := time.ParseDuration(strings.TrimSpace(f))
		if err != nil || d <= 0 {
			return fmt.Errorf("bad lease %q", f)
		}
		leases = append(leases, d)
		p.Leases = append(p.Leases, d.String())
	}
	p.Poll, p.KillPoint, p.KillJitter = poll.String(), string(benchwork.KillDuringCall), jit.String()
	p.MockMedian, p.MaxAttempts = median.String(), 5

	ctx := context.Background()
	e, err := benchwork.CaptureEnv(ctx, ownerURL)
	if err != nil {
		return err
	}
	if e.Dirty && !allowDirty {
		return errors.New("working tree has uncommitted changes; commit first so the recorded commit hash is the code that ran (or --allow-dirty for a smoke run)")
	}

	tmp, err := os.MkdirTemp("", "recovery-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "benchworker")
	if b, err := exec.Command("go", "build", "-o", bin, "./cmd/benchworker").CombinedOutput(); err != nil {
		return fmt.Errorf("build benchworker: %v\n%s", err, b)
	}

	rep := report{StartedAt: time.Now().UTC(), Params: p, Env: e}
	for _, lease := range leases {
		cr := configReport{Lease: lease.String()}
		for i := 1; i <= p.Trials; i++ {
			fmt.Fprintf(os.Stderr, "lease %v trial %d/%d\n", lease, i, p.Trials)
			res, err := benchwork.RunRecovery(ctx, benchwork.RecoveryParams{
				DatabaseURL: appURL, OwnerURL: ownerURL, WorkerBin: bin,
				Workers: p.Workers, Concurrency: p.Concurrency, Jobs: p.Jobs,
				Lease: lease, Poll: poll, Kill: benchwork.KillDuringCall, KillJitter: jit,
				Mock:         mockllm.Config{LatencyMedian: median, LatencySigma: p.MockSigma, Seed: uint64(i)},
				DrainTimeout: 5 * time.Minute,
			})
			if err != nil {
				return fmt.Errorf("lease %v trial %d: %w", lease, i, err)
			}
			cr.Trials = append(cr.Trials, res)
		}
		cr.Summary = summarize(cr.Trials)
		rep.Configs = append(rep.Configs, cr)
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	base := filepath.Join(out, "recovery-"+rep.StartedAt.Format("20060102T150405Z"))
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(base+".json", append(raw, '\n'), 0o644); err != nil {
		return err
	}
	text := render(rep)
	if err := os.WriteFile(base+".txt", []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Print(text)
	fmt.Println("wrote", base+".json and .txt")
	for _, c := range rep.Configs {
		if s := c.Summary; s.LostJobs != 0 || s.DuplicateFx != 0 || s.MissingFx != 0 {
			return errors.New("ANOMALIES FOUND: see the result files")
		}
	}
	return nil
}

func summarize(trials []benchwork.RecoveryResult) summary {
	s := summary{Trials: len(trials)}
	var per, perTrial []float64
	for _, t := range trials {
		s.Jobs += t.Jobs
		s.Succeeded += t.Succeeded
		s.LostJobs += t.LostJobs
		s.Dead += t.Dead
		s.DuplicateFx += t.DuplicateFx
		s.MissingFx += t.MissingFx
		s.Orphans += t.Orphans
		s.ExtraAttempts += t.ExtraAttempts
		if t.Orphans == 0 {
			s.TrialsNoOrphan++
			continue
		}
		worst, unfinished := 0.0, false
		for _, secs := range t.RecoverySecs {
			if secs < 0 { // never finished: counted as lost, kept out of the timing
				unfinished = true
				continue
			}
			per = append(per, secs)
			worst = math.Max(worst, secs)
		}
		if !unfinished {
			perTrial = append(perTrial, worst)
		}
	}
	s.PerOrphanSecs, s.PerTrialSecs = benchwork.Distribution(per), benchwork.Distribution(perTrial)
	return s
}

func render(r report) string {
	var b strings.Builder
	p, e := r.Params, r.Env
	fmt.Fprintf(&b, "Crash-recovery benchmark: kill -9 of one worker process mid-job\n")
	fmt.Fprintf(&b, "Workload: mock LLM with simulated latency of %s median (log-normal sigma %.2f); %d jobs per trial, %d workers x %d slots, poll %s, kill point %q after a random delay in [0, %s), max_attempts %d\n",
		p.MockMedian, p.MockSigma, p.Jobs, p.Workers, p.Concurrency, p.Poll, p.KillPoint, p.KillJitter, p.MaxAttempts)
	fmt.Fprintf(&b, "Commit %s (dirty: %v), %s, %d CPUs, %s bytes RAM, %s, %s\n", e.Commit, e.Dirty, e.Machine, e.CPUs, e.MemoryBytes, e.OS, e.GoVersion)
	fmt.Fprintf(&b, "Postgres: %s\nDocker postgres container: %s\nDocker engine: %s\nLoad average at start: %s\n\n", e.Postgres, e.Docker, e.DockerInfo, e.LoadAvg)
	for _, c := range r.Configs {
		s := c.Summary
		fmt.Fprintf(&b, "lease %s: %d trials, %d jobs, %d succeeded, %d lost, %d dead, %d duplicate effects, %d missing effects\n",
			c.Lease, s.Trials, s.Jobs, s.Succeeded, s.LostJobs, s.Dead, s.DuplicateFx, s.MissingFx)
		fmt.Fprintf(&b, "  orphans (jobs the killed worker held): %d total (%d trials had none); extra LLM calls begun: %d\n", s.Orphans, s.TrialsNoOrphan, s.ExtraAttempts)
		fmt.Fprintf(&b, "  recovery, per orphan (kill -> job succeeded), seconds: n=%d min %.2f median %.2f p90 %.2f max %.2f\n",
			s.PerOrphanSecs.N, s.PerOrphanSecs.Min, s.PerOrphanSecs.Median, s.PerOrphanSecs.P90, s.PerOrphanSecs.Max)
		fmt.Fprintf(&b, "  recovery, per trial (last orphan done), seconds: n=%d min %.2f median %.2f p90 %.2f max %.2f\n\n",
			s.PerTrialSecs.N, s.PerTrialSecs.Min, s.PerTrialSecs.Median, s.PerTrialSecs.P90, s.PerTrialSecs.Max)
	}
	return b.String()
}
