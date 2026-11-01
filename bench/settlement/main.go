// Command settlement runs the settlement fault-injection benchmark (DESIGN.md section 10,
// item 5): settlements arrive as a Poisson process and are executed by real worker
// processes against a real mock-payment-provider process that loses responses, never
// receives requests, declines, and sends duplicated, delayed, dropped and reordered
// signed webhooks, with a worker killed with SIGKILL in one mix. After each run the
// ledger is compared with the provider's own record of every payment. Settlement is
// SIMULATED: every result says "mock payment provider with simulated latency of X ms".
//
//	go run ./bench/settlement --runs 3 --settlements 300 --out bench/results
//
// Each run uses a scratch database that is created and dropped, so it starts from an
// empty jobs table whatever the development database holds. Settlements are inserted
// as the create-settlement handler inserts them (settlement, execute job and audit event
// in one transaction); the API's authentication and capacity check are not involved or
// measured.
//
// It writes <out>/settlement-<timestamp>.json (every run, the parameters and the
// environment) and a .txt summary, and refuses a dirty working tree unless --allow-dirty
// (smoke runs only; do not commit those). It exits non-zero if any run had an anomaly,
// an unresolved settlement or a dead job.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/riyne/fifty-fifty/internal/benchwork"
	"github.com/riyne/fifty-fifty/internal/settlerig"
)

const webhookSecret = "whsec_bench"

// misbehaving is how the provider's webhooks misbehave in the mixes that have webhooks
// turned on and are about misbehavior.
var misbehaving = []string{"--webhook-duplicate", "0.3", "--webhook-delay-max", "200ms", "--webhook-out-of-order", "0.3"}

func mixes() []settlerig.Mix {
	cat := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	lostAfter := []string{"--rate-timeout-after", "0.1", "--rate-5xx-after", "0.1"}
	return []settlerig.Mix{
		{Name: "no faults", Webhooks: true},
		{Name: "lost responses, webhooks delivered", Mock: cat(lostAfter, misbehaving), Webhooks: true},
		{Name: "lost responses, no webhooks", Mock: lostAfter},
		{Name: "requests never arrive", Mock: cat([]string{"--rate-timeout-before", "0.1", "--rate-5xx-before", "0.1"}, misbehaving), Webhooks: true},
		{Name: "everything, one worker killed", Webhooks: true, KillOne: true, Mock: []string{
			"--rate-decline", "0.05", "--rate-timeout-before", "0.05", "--rate-5xx-before", "0.05", "--rate-timeout-after", "0.05", "--rate-5xx-after", "0.05",
			"--rate-read-error", "0.1", "--webhook-drop", "0.2", "--webhook-duplicate", "0.3", "--webhook-delay-max", "300ms", "--webhook-out-of-order", "0.3"}},
	}
}

type params struct {
	Runs           int     `json:"runs_per_mix"`
	Settlements    int     `json:"settlements_per_run"`
	Rate           float64 `json:"arrivals_per_second"`
	Workers        int     `json:"worker_processes"`
	Concurrency    int     `json:"jobs_in_flight_per_worker"`
	Lease          string  `json:"lease"`
	Poll           string  `json:"poll_interval"`
	Latency        string  `json:"mock_payment_provider_latency_median"`
	LatencySigma   float64 `json:"mock_payment_provider_latency_sigma"`
	TimeoutHold    string  `json:"timeout_fault_hold"`
	ReconcileFirst string  `json:"first_reconcile_delay_placeholder"`
	ReconcileMax   string  `json:"max_reconcile_delay_placeholder"`
	ReconcileCheck int     `json:"max_reconcile_checks_placeholder"`
}

type mixReport struct {
	Mix  settlerig.Mix          `json:"mix"`
	Runs []settlerig.LoadResult `json:"runs"`
}

type report struct {
	StartedAt time.Time     `json:"started_at"`
	Params    params        `json:"parameters"`
	Env       benchwork.Env `json:"environment"`
	Mixes     []mixReport   `json:"results"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "settlement benchmark:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	var (
		p          params
		out        string
		only       string
		allowDirty bool
		lease      time.Duration
		poll       time.Duration
		hold       time.Duration
		median     time.Duration
		timeout    time.Duration
	)
	flag.IntVar(&p.Runs, "runs", 3, "runs per mix (each with its own seed and scratch database)")
	flag.IntVar(&p.Settlements, "settlements", 300, "settlements per run")
	flag.Float64Var(&p.Rate, "rate", 8, "settlement arrivals per second (Poisson)")
	flag.IntVar(&p.Workers, "workers", 3, "worker processes")
	flag.IntVar(&p.Concurrency, "concurrency", 4, "jobs in flight per worker")
	flag.DurationVar(&lease, "lease", 5*time.Second, "worker lease")
	flag.DurationVar(&poll, "poll", time.Second, "worker poll interval (the worker default)")
	flag.DurationVar(&median, "latency-median", 300*time.Millisecond, "mock payment provider median latency")
	flag.Float64Var(&p.LatencySigma, "latency-sigma", 0.4, "mock payment provider latency sigma")
	flag.DurationVar(&hold, "timeout-hold", 3*time.Second, "how long the provider holds a timed-out connection before aborting it (the worker's own client timeout is 10 s)")
	flag.DurationVar(&timeout, "resolve-timeout", 10*time.Minute, "how long to wait for every settlement to be final")
	flag.StringVar(&only, "mixes", "", "comma-separated 1-based mix numbers to run (default all; for smoke runs)")
	flag.StringVar(&out, "out", "bench/results", "directory for the result files")
	flag.BoolVar(&allowDirty, "allow-dirty", false, "run on a working tree with uncommitted changes (smoke runs only)")
	flag.Parse()

	ownerURL, appURL := os.Getenv("MIGRATE_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		return errors.New("MIGRATE_DATABASE_URL and DATABASE_URL are required (the Makefile sets them)")
	}
	p.Lease, p.Poll, p.Latency, p.TimeoutHold = lease.String(), poll.String(), median.String(), hold.String()
	p.ReconcileFirst, p.ReconcileMax, p.ReconcileCheck = "5s", "5m0s", 12

	// The environment is captured from a scratch database: that is the database every run
	// uses, so its (empty) jobs table is what the runs started with.
	envDB, err := settlerig.NewScratch(ctx, ownerURL, appURL)
	if err != nil {
		return err
	}
	e, err := benchwork.CaptureEnv(ctx, envDB.OwnerURL)
	_ = envDB.Drop(ctx)
	if err != nil {
		return err
	}
	if e.Dirty && !allowDirty {
		return errors.New("working tree has uncommitted changes; commit first so the recorded commit hash is the code that ran (or --allow-dirty for a smoke run)")
	}

	tmp, err := os.MkdirTemp("", "settlement-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bins, err := settlerig.Build(ctx, tmp)
	if err != nil {
		return err
	}

	all := mixes()
	selected := all
	if only != "" {
		selected = nil
		for _, f := range strings.Split(only, ",") {
			var n int
			if _, err := fmt.Sscanf(strings.TrimSpace(f), "%d", &n); err != nil || n < 1 || n > len(all) {
				return fmt.Errorf("--mixes: %q is not a mix number 1-%d", f, len(all))
			}
			selected = append(selected, all[n-1])
		}
	}

	rep := report{StartedAt: time.Now().UTC(), Params: p, Env: e}
	for _, mix := range selected {
		mr := mixReport{Mix: mix}
		for i := 1; i <= p.Runs; i++ {
			fmt.Fprintf(os.Stderr, "%s: run %d/%d\n", mix.Name, i, p.Runs)
			mock := append([]string{"--latency-median", median.String(), "--latency-sigma", fmt.Sprint(p.LatencySigma)}, mix.Mock...)
			mix2 := mix
			mix2.Mock = mock
			res, err := settlerig.RunLoad(ctx, settlerig.LoadParams{
				OwnerURL: ownerURL, AppURL: appURL, Bins: bins, Mix: mix2,
				Settlements: p.Settlements, Rate: p.Rate, Workers: p.Workers, Concurrency: p.Concurrency,
				Lease: lease, Poll: poll, TimeoutHold: hold, Seed: uint64(i), WebhookSecret: webhookSecret,
				ResolveTimeout: timeout, Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
			})
			if err != nil {
				return fmt.Errorf("%s run %d: %w", mix.Name, i, err)
			}
			mr.Runs = append(mr.Runs, res)
		}
		rep.Mixes = append(rep.Mixes, mr)
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	base := filepath.Join(out, "settlement-"+rep.StartedAt.Format("20060102T150405Z"))
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

	for _, m := range rep.Mixes {
		for _, r := range m.Runs {
			if len(r.Anomalies) != 0 || r.Unresolved != 0 || r.DeadJobs != 0 || r.Postings != r.ByStatus["succeeded"] || r.Charges != r.ByStatus["succeeded"] {
				return fmt.Errorf("%s (seed %d): anomalies=%d unresolved=%d dead=%d postings=%d charges=%d succeeded=%d",
					r.Mix, r.Seed, len(r.Anomalies), r.Unresolved, r.DeadJobs, r.Postings, r.Charges, r.ByStatus["succeeded"])
			}
		}
	}
	return nil
}

func render(rep report) string {
	var b strings.Builder
	p, e := rep.Params, rep.Env
	fmt.Fprintf(&b, "Settlement fault-injection benchmark (simulated settlement: mock payment provider with simulated latency of %s median, log-normal sigma %.1f)\n", p.Latency, p.LatencySigma)
	fmt.Fprintf(&b, "started %s, commit %s (working tree dirty: %v)\n", rep.StartedAt.Format(time.RFC3339), e.Commit, e.Dirty)
	fmt.Fprintf(&b, "machine: %s, %d CPUs, %s bytes RAM, %s; %s\n", e.Machine, e.CPUs, e.MemoryBytes, e.OS, e.Postgres)
	fmt.Fprintf(&b, "docker: %s; engine %s\nload average at start: %s\n", e.Docker, e.DockerInfo, e.LoadAvg)
	fmt.Fprintf(&b, "per run: %d settlements arriving at %.0f/s (Poisson), %d worker processes x %d jobs in flight, lease %s, poll %s, timeout faults held %s then aborted; scratch database per run (jobs table empty at start)\n",
		p.Settlements, p.Rate, p.Workers, p.Concurrency, p.Lease, p.Poll, p.TimeoutHold)
	fmt.Fprintf(&b, "reconcile schedule (placeholders, not tuned): first check %s, doubling to %s, %d checks. Times of resolving an unknown settlement measure THESE constants.\n\n",
		p.ReconcileFirst, p.ReconcileMax, p.ReconcileCheck)

	for _, m := range rep.Mixes {
		fmt.Fprintf(&b, "== %s (%s)\n", m.Mix.Name, strings.Join(m.Mix.Mock, " "))
		var p50, p99, unkMed []float64
		for _, r := range m.Runs {
			s := r.Provider
			fmt.Fprintf(&b, "run seed %d: final %v, unresolved %d, anomalies %d | postings %d, provider charges %d | dead jobs %d, execute jobs retried %d, reconcile jobs %d, watchdog requeues %d, worker killed %v\n",
				r.Seed, r.ByStatus, r.Unresolved, len(r.Anomalies), r.Postings, r.Charges, r.DeadJobs, r.ExecuteJobsRetried, r.ReconcileJobs, r.WatchdogRequeues, r.KilledWorker)
			fmt.Fprintf(&b, "    end to end s: p50 %.2f p90 %.2f p99 %.2f max %.2f (n=%d) | went unknown: %d, unknown to final s: min %.1f median %.1f p90 %.1f max %.1f\n",
				r.EndToEnd.Median, r.EndToEnd.P90, r.EndToEnd.P99, r.EndToEnd.Max, r.EndToEnd.N, r.WentUnknown,
				r.UnknownToFinal.Min, r.UnknownToFinal.Median, r.UnknownToFinal.P90, r.UnknownToFinal.Max)
			fmt.Fprintf(&b, "    provider saw: %d create calls (%d replays), %d payments (%d charges, %d declined); timeouts before/after %d/%d, 5xx before/after %d/%d; reads %d (%d failed); webhooks: %d events, %d dropped, %d duplicated, %d reordered payments, %d deliveries (%d refused or failed)\n",
				s.CreateCalls, s.Replays, s.Payments, s.Charges, s.Declined, s.TimeoutBefore, s.TimeoutAfter, s.Errors5xxBef, s.Errors5xxAft,
				s.Reads, s.ReadErrors, s.WebhookEvents, s.WebhookDropped, s.WebhookDuplicates, s.WebhookOutOfOrder, s.WebhookDeliveries, s.WebhookNon2xxOrFail)
			for _, a := range r.Anomalies {
				fmt.Fprintf(&b, "    ANOMALY: %s\n", a)
			}
			p50, p99 = append(p50, r.EndToEnd.Median), append(p99, r.EndToEnd.P99)
			if r.WentUnknown > 0 {
				unkMed = append(unkMed, r.UnknownToFinal.Median)
			}
		}
		fmt.Fprintf(&b, "over %d runs (median [min-max] of the per-run value): end-to-end p50 %s s, p99 %s s", len(m.Runs), spread(p50), spread(p99))
		if len(unkMed) > 0 {
			fmt.Fprintf(&b, "; unknown-to-final median %s s", spread(unkMed))
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

// spread formats the median of xs with its range.
func spread(xs []float64) string {
	if len(xs) == 0 {
		return "n/a"
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return fmt.Sprintf("%.2f [%.2f-%.2f]", s[(len(s)-1)/2], s[0], s[len(s)-1])
}
