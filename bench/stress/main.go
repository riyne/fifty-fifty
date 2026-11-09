// Command stress runs the ledger stress benchmark and its
// negative controls.
//
// A mixed concurrent workload (create, claim, unclaim, finalize, void, balances reads,
// settle) runs against the real api with real tokens, real worker processes and a mock
// payment provider, with retries, duplicate idempotency keys, abandoned requests and
// barrier-released races. Afterwards every invariant is recomputed from the database
// tables and the provider's own record, never from API responses. Settlement is
// SIMULATED: every result says "mock payment provider with simulated latency of X ms".
//
// Two builds, two kinds of run:
//
//	go run ./bench/stress                              # the production build, nothing disabled
//	go run -tags unsafe_controls ./bench/stress        # the negative controls, one protection off at a time
//
// The production configuration must show zero violations, and must prove the dangerous
// things happened (retries replayed, races met, locks waited on). Each control must show
// what disabling that protection breaks, or the database layer that caught it, exactly as
// make test-controls expects. The program writes <out>/stress-<build>-<timestamp>.json
// (every run, parameters, environment) and a .txt summary, refuses a dirty working tree
// unless --allow-dirty (smoke runs only; do not commit those), and exits non-zero if the
// production configuration is not clean or a control does not match its expectation (the
// files are written first: a recording is never deleted).
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

	"github.com/riyne/fifty-fifty/internal/harness/benchwork"
	"github.com/riyne/fifty-fifty/internal/harness/settlerig"
	"github.com/riyne/fifty-fifty/internal/harness/stressrig"
)

const webhookSecret = "whsec_stress"

type params struct {
	Runs          int    `json:"runs_per_configuration"`
	Duration      string `json:"workload_duration"`
	Users         int    `json:"users"`
	Groups        int    `json:"expense_groups"`
	RaceLoops     int    `json:"race_loops_per_group"`
	SettleWorkers int    `json:"settle_round_goroutines"`
	RoundPause    string `json:"settle_round_pause"`
	Workers       int    `json:"worker_processes"`
	Concurrency   int    `json:"jobs_in_flight_per_worker"`
	Lease         string `json:"lease"`
	Poll          string `json:"poll_interval"`
	TimeoutHold   string `json:"timeout_fault_hold"`
	APIPoolConns  int    `json:"api_pool_connections"`
	Latency       string `json:"mock_payment_provider_latency_median"`
	ProductionMix string `json:"mock_payment_provider_flags_default"`
	ResolveMix    string `json:"mock_payment_provider_flags_resolve_race"`
}

type runReport struct {
	Seed         uint64           `json:"seed"`
	Clean        bool             `json:"clean"`
	Problems     []string         `json:"problems,omitempty"`
	Vacuous      []string         `json:"vacuous,omitempty"`
	ServerErrors []string         `json:"server_errors,omitempty"`
	Result       stressrig.Result `json:"result"`
}

type configReport struct {
	Name        string      `json:"name"`
	Controls    []string    `json:"protections_disabled"`
	IndexDrop   bool        `json:"posting_index_dropped"`
	ResolveRace bool        `json:"resolve_race_mock"`
	Runs        []runReport `json:"runs"`
}

type report struct {
	StartedAt time.Time      `json:"started_at"`
	Build     string         `json:"build"`
	Params    params         `json:"parameters"`
	Env       benchwork.Env  `json:"environment"`
	Configs   []configReport `json:"results"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "stress benchmark:", err)
		os.Exit(1)
	}
}

// config is a named run configuration: production, or one control.
type config struct {
	name string
	cc   *stressrig.ControlCase // nil for production
}

func run() error {
	ctx := context.Background()
	var (
		p                            params
		out, only                    string
		allowDirty                   bool
		duration, pause, lease, poll time.Duration
		hold, resolve                time.Duration
	)
	flag.IntVar(&p.Runs, "runs", 3, "runs per configuration (seeds 1..runs, each on its own scratch database)")
	flag.DurationVar(&duration, "duration", 60*time.Second, "how long the workload runs (K); settlements then drain")
	flag.IntVar(&p.Users, "users", 24, "users in the expense groups (N)")
	flag.IntVar(&p.Groups, "groups", 4, "expense groups (G)")
	flag.IntVar(&p.RaceLoops, "race-loops", 2, "barrier-released race loops per expense group")
	flag.IntVar(&p.SettleWorkers, "settle-workers", 3, "goroutines running settle rounds (a fresh group per round)")
	flag.DurationVar(&pause, "round-pause", 300*time.Millisecond, "pause between a settle goroutine's rounds")
	flag.IntVar(&p.Workers, "workers", 3, "worker processes")
	flag.IntVar(&p.Concurrency, "concurrency", 4, "jobs in flight per worker")
	flag.DurationVar(&lease, "lease", 5*time.Second, "worker lease")
	flag.DurationVar(&poll, "poll", time.Second, "worker poll interval (the worker default)")
	flag.DurationVar(&hold, "timeout-hold", 3*time.Second, "how long the provider holds a timed-out connection before aborting it")
	flag.DurationVar(&resolve, "resolve-timeout", 10*time.Minute, "how long to wait for every settlement to be final")
	flag.IntVar(&p.APIPoolConns, "api-pool", 30, "connections the in-process api may use")
	flag.StringVar(&only, "configs", "", "comma-separated 1-based configuration numbers to run (default all available in this build)")
	flag.StringVar(&out, "out", "bench/results", "directory for the result files")
	flag.BoolVar(&allowDirty, "allow-dirty", false, "run on a working tree with uncommitted changes (smoke runs only)")
	flag.Parse()

	ownerURL, appURL := os.Getenv("MIGRATE_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		return errors.New("MIGRATE_DATABASE_URL and DATABASE_URL are required (the Makefile sets them)")
	}
	p.Duration, p.RoundPause, p.Lease, p.Poll, p.TimeoutHold = duration.String(), pause.String(), lease.String(), poll.String(), hold.String()
	p.Latency = stressrig.MockLatency
	p.ProductionMix, p.ResolveMix = strings.Join(stressrig.DefaultMockFlags, " "), strings.Join(stressrig.ResolveRaceMockFlags, " ")

	build := "production"
	configs := []config{{name: "production (nothing disabled)"}}
	if stressrig.UnsafeControlsBuilt {
		build = "controls"
		configs = nil
		for i := range stressrig.ControlCases {
			configs = append(configs, config{name: stressrig.ControlCases[i].Name, cc: &stressrig.ControlCases[i]})
		}
	}
	if only != "" {
		all := configs
		configs = nil
		for _, f := range strings.Split(only, ",") {
			var n int
			if _, err := fmt.Sscanf(strings.TrimSpace(f), "%d", &n); err != nil || n < 1 || n > len(all) {
				return fmt.Errorf("--configs: %q is not a configuration number 1-%d in this build", f, len(all))
			}
			configs = append(configs, all[n-1])
		}
	}

	// The environment is captured from a scratch database, which is what every run uses.
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

	tmp, err := os.MkdirTemp("", "stress-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	tags := ""
	if stressrig.UnsafeControlsBuilt {
		tags = "unsafe_controls" // the workers must carry the same switches as this process
	}
	bins, err := settlerig.BuildWithTags(ctx, tmp, tags)
	if err != nil {
		return err
	}

	rep := report{StartedAt: time.Now().UTC(), Build: build, Params: p, Env: e}
	var failures []string
	for _, cfg := range configs {
		cr := configReport{Name: cfg.name}
		for i := 1; i <= p.Runs; i++ {
			fmt.Fprintf(os.Stderr, "%s: run %d/%d\n", cfg.name, i, p.Runs)
			rp := stressrig.Params{
				OwnerURL: ownerURL, AppURL: appURL, Bins: bins, Seed: uint64(i), Duration: duration,
				Users: p.Users, Groups: p.Groups, RaceLoops: p.RaceLoops,
				SettleWorkers: p.SettleWorkers, RoundPause: pause,
				Workers: p.Workers, WorkerConcurrency: p.Concurrency, Lease: lease, Poll: poll,
				MockFlags: stressrig.DefaultMockFlags, TimeoutHold: hold, WebhookSecret: webhookSecret,
				ResolveTimeout: resolve, APIPoolConns: p.APIPoolConns,
				Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
			}
			if cfg.cc != nil {
				rp = cfg.cc.Apply(rp)
				cr.Controls, cr.IndexDrop, cr.ResolveRace = cfg.cc.Off, cfg.cc.DropIndex, cfg.cc.ResolveRace
			}
			res, err := stressrig.Run(ctx, rp)
			if err != nil {
				return fmt.Errorf("%s run %d: %w", cfg.name, i, err)
			}
			rr := runReport{Seed: uint64(i), Result: res, Vacuous: res.Vacuous(rp), ServerErrors: res.ServerErrors()}
			if cfg.cc != nil {
				rr.Problems = cfg.cc.Problems(res, rp)
			} else {
				rr.Clean = res.Clean(rp)
				if !rr.Clean {
					rr.Problems = append(rr.Problems, "production run is not clean")
				}
			}
			rr.Clean = len(rr.Problems) == 0
			if !rr.Clean {
				failures = append(failures, fmt.Sprintf("%s (seed %d): %s", cfg.name, i, strings.Join(rr.Problems, "; ")))
			}
			cr.Runs = append(cr.Runs, rr)
		}
		rep.Configs = append(rep.Configs, cr)
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	base := filepath.Join(out, "stress-"+build+"-"+rep.StartedAt.Format("20060102T150405Z"))
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
	if len(failures) > 0 {
		return fmt.Errorf("%d run(s) did not match expectations:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}

func render(rep report) string {
	var b strings.Builder
	p, e := rep.Params, rep.Env
	fmt.Fprintf(&b, "Ledger stress benchmark, %s build (simulated settlement: mock payment provider with simulated latency of %s median, log-normal sigma 0.4)\n", rep.Build, p.Latency)
	fmt.Fprintf(&b, "started %s, commit %s (working tree dirty: %v)\n", rep.StartedAt.Format(time.RFC3339), e.Commit, e.Dirty)
	fmt.Fprintf(&b, "machine: %s, %d CPUs, %s bytes RAM, %s; %s\n", e.Machine, e.CPUs, e.MemoryBytes, e.OS, e.Postgres)
	fmt.Fprintf(&b, "docker: %s; engine %s\nload average at start: %s\n", e.Docker, e.DockerInfo, e.LoadAvg)
	fmt.Fprintf(&b, "per run: %d users in %d expense groups acting for %s (each user a loop of create/claim/unclaim/finalize/void/balances; %d barrier-race loops per group); %d goroutines running settle rounds (a fresh group of 6 per round, %s apart); then settlements drain.\n",
		p.Users, p.Groups, p.Duration, p.RaceLoops, p.SettleWorkers, p.RoundPause)
	fmt.Fprintf(&b, "api pool %d connections; %d worker processes x %d jobs in flight, lease %s, poll %s; provider timeout faults held %s; a scratch database per run; seeds 1..%d (the seed fixes each goroutine's sequence of operations, not the interleaving)\n",
		p.APIPoolConns, p.Workers, p.Concurrency, p.Lease, p.Poll, p.TimeoutHold, p.Runs)
	fmt.Fprintf(&b, "mock provider flags: %s\n  (resolve-race cases use: %s)\n\n", p.ProductionMix, p.ResolveMix)

	for _, c := range rep.Configs {
		fmt.Fprintf(&b, "== %s\n", c.Name)
		if len(c.Controls) > 0 || c.IndexDrop {
			fmt.Fprintf(&b, "protections disabled: %v; unique posting index dropped: %v\n", c.Controls, c.IndexDrop)
		} else {
			b.WriteString("protections disabled: none\n")
		}
		perInv := map[stressrig.Invariant][]int{}
		for _, r := range c.Runs {
			res := r.Result
			verdict := "matches expectation"
			if !r.Clean {
				verdict = "DOES NOT MATCH: " + strings.Join(r.Problems, "; ")
			}
			fmt.Fprintf(&b, "run seed %d: %s (%.0f s)\n", r.Seed, verdict, res.Elapsed)
			fmt.Fprintf(&b, "    checked: %d expenses (%d finalized, %d voided), %d settlements (%d with a posting), %d logical creates, %d ledger transactions; settlements unresolved %d, succeeded %d\n",
				res.Checked.Expenses, res.Checked.Finalized, res.Checked.Voided, res.Checked.Settlements, res.Checked.SettlementPostings, res.Checked.Creates, res.Checked.LedgerTransactions,
				res.Unresolved, res.Counts["settlements_succeeded"])
			var v []string
			for _, inv := range stressrig.Invariants {
				perInv[inv] = append(perInv[inv], res.Violations[inv])
				if res.Violations[inv] > 0 {
					v = append(v, fmt.Sprintf("%s=%d", inv, res.Violations[inv]))
				}
			}
			if len(v) == 0 {
				v = []string{"none"}
			}
			fmt.Fprintf(&b, "    invariants violated: %s\n", strings.Join(v, " "))
			fmt.Fprintf(&b, "    clients saw: 5xx %d, settle accepted %d / refused for capacity %d / replayed %d, finalize ok %d, void ok %d, claims refused after finalize %d, requests abandoned %d, races: %d rounds, %d with a finalize landing mid-burst\n",
				res.Counts["http_5xx"], res.Counts["settle/202"], res.Counts["refused/settle"], res.Counts["replayed/settle"], res.Counts["finalize/200"], res.Counts["void/200"],
				res.Counts["claim/409_not_claimable"]+res.Counts["unclaim/409_not_claimable"],
				res.Counts["abandoned/create"]+res.Counts["abandoned/finalize"]+res.Counts["abandoned/void"]+res.Counts["abandoned/settle"],
				res.Counts["race_rounds"], res.Counts["race_mixed"])
			fmt.Fprintf(&b, "    retries answered from the stored response: create %d finalize %d void %d settle %d; concurrent duplicates that waited and replayed: create %d finalize %d void %d settle %d\n",
				res.Counts["replayed/create"], res.Counts["replayed/finalize"], res.Counts["replayed/void"], res.Counts["replayed/settle"],
				res.Counts["duplicate_waited_and_replayed/create"], res.Counts["duplicate_waited_and_replayed/finalize"], res.Counts["duplicate_waited_and_replayed/void"], res.Counts["duplicate_waited_and_replayed/settle"])
			fmt.Fprintf(&b, "    database lock waits seen by a 20 ms sampler: %d of %d samples had a backend waiting (at most %d)\n",
				res.Locks.SamplesWaiting, res.Locks.Samples, res.Locks.MaxWaiting)
			if s := res.Provider; s != nil {
				fmt.Fprintf(&b, "    provider saw: %d create calls (%d replays), %d payments (%d charges, %d declined); timeouts before/after %d/%d, 5xx before/after %d/%d; webhooks: %d events, %d dropped, %d duplicated, %d reordered payments, %d deliveries\n",
					s.CreateCalls, s.Replays, s.Payments, s.Charges, s.Declined, s.TimeoutBefore, s.TimeoutAfter, s.Errors5xxBef, s.Errors5xxAft,
					s.WebhookEvents, s.WebhookDropped, s.WebhookDuplicates, s.WebhookOutOfOrder, s.WebhookDeliveries)
			}
			b.WriteString(classes("api logged", stressrig.LogErrorPrefix, res.Counts))
			b.WriteString(classes("workers logged", stressrig.WorkerLogErrorPrefix, res.Counts))
			if len(res.Unexpected) > 0 {
				fmt.Fprintf(&b, "    unexpected responses (first %d shown): %s\n", min(3, len(res.Unexpected)), strings.Join(res.Unexpected[:min(3, len(res.Unexpected))], " | "))
			}
			for _, a := range res.Examples {
				d := a.Detail
				if len(d) > 220 {
					d = d[:220] + "..."
				}
				fmt.Fprintf(&b, "    e.g. %s: %s\n", a.Invariant, d)
			}
		}
		fmt.Fprintf(&b, "violations per run, in order of seeds:")
		for _, inv := range stressrig.Invariants {
			fmt.Fprintf(&b, " %s=%v", inv, perInv[inv])
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

// classes prints the logged error classes with a prefix, apart from clients going away.
func classes(label, prefix string, counts map[string]int64) string {
	var keys []string
	away := int64(0)
	for k := range counts {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if strings.HasPrefix(strings.TrimPrefix(k, prefix), "client_gone") {
			away += counts[k]
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "    %s: %d from clients going away", label, away)
	if len(keys) == 0 {
		b.WriteString("; no other error\n")
		return b.String()
	}
	b.WriteString("; other errors:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "        %s = %d\n", strings.TrimPrefix(k, prefix), counts[k])
	}
	return b.String()
}
