// Command rls measures what row-level security and the identity seam cost (docs/EVIDENCE.md,
// claim 10): the same seeded database and the same closed-loop mix of the handlers'
// queries, run with row-level security disabled on the plain pool (the access path before
// the policies existed), disabled through the identity seam, and enabled through it.
// The first difference is the cost of the seam, the second the cost of the policies.
//
//	go run ./bench/rls --reps 6 --out bench/results
//
// Needs MIGRATE_DATABASE_URL (the owner) and DATABASE_URL (the application role), which the
// Makefile exports, a clean working tree (unless --allow-dirty, smoke runs only; do not
// commit those) and a quiet machine. Each repetition uses a fresh scratch database. It
// writes <out>/rls-<timestamp>.json (every repetition, the parameters and the environment)
// and a .txt summary, and exits non-zero if any control failed or any operation failed.
// Everything is on one machine: the workers, Postgres in Docker and the harness share it.
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
	"github.com/riyne/fifty-fifty/internal/rlsbench"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "rls benchmark:", err)
		os.Exit(1)
	}
}

type report struct {
	StartedAt time.Time            `json:"started_at"`
	Params    map[string]any       `json:"parameters"`
	Env       benchwork.Env        `json:"environment"`
	Reps      []rlsbench.Rep       `json:"repetitions"`
	Summary   map[string]any       `json:"summary"`
	Ratios    map[string][]float64 `json:"throughput_ratios_per_repetition"`
}

func run() error {
	var p rlsbench.Params
	var out string
	var allowDirty bool
	flag.IntVar(&p.Groups, "groups", 200, "groups")
	flag.IntVar(&p.MembersPerGroup, "members", 6, "members per group (every fifth user is also in a second group)")
	flag.IntVar(&p.ExpensesPerGroup, "expenses", 200, "expenses per group, half finalized with a posting, half claiming")
	flag.IntVar(&p.Workers, "workers", 8, "closed-loop workers (and connections)")
	flag.DurationVar(&p.Warmup, "warmup", 3*time.Second, "warm-up before each timed phase")
	flag.DurationVar(&p.Measure, "measure", 15*time.Second, "timed phase per variant")
	flag.IntVar(&p.Reps, "reps", 6, "repetitions, each on a fresh database, variants in a rotating order")
	flag.Uint64Var(&p.Seed, "seed", 1, "seed of the operation sequences")
	flag.StringVar(&out, "out", "bench/results", "directory for the result files")
	flag.BoolVar(&allowDirty, "allow-dirty", false, "run on a working tree with uncommitted changes (smoke runs only)")
	flag.Parse()

	p.OwnerURL, p.AppURL = os.Getenv("MIGRATE_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if p.OwnerURL == "" || p.AppURL == "" {
		return errors.New("MIGRATE_DATABASE_URL (owner) and DATABASE_URL (app role) are required; the Makefile exports both")
	}
	ctx := context.Background()
	env, err := benchwork.CaptureEnv(ctx, p.OwnerURL)
	if err != nil {
		return err
	}
	if env.Dirty && !allowDirty {
		return errors.New("working tree has uncommitted changes; commit first so the recorded commit hash is the code that ran (or --allow-dirty for a smoke run)")
	}
	p.Logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	started := time.Now().UTC()
	reps, runErr := rlsbench.Run(ctx, p)

	rep := report{
		StartedAt: started, Env: env, Reps: reps,
		Params: map[string]any{
			"groups": p.Groups, "members_per_group": p.MembersPerGroup, "expenses_per_group": p.ExpensesPerGroup,
			"workers": p.Workers, "warmup": p.Warmup.String(), "measure_per_variant": p.Measure.String(), "repetitions": p.Reps, "seed": p.Seed,
			"mix_percent": "list_groups 10, list_expenses 25, get_expense 30, balances 20, claim_unclaim 15 (each claim_unclaim is two transactions: add the claim, remove it)",
		},
	}
	rep.Ratios = ratios(reps)
	rep.Summary = summarize(reps)
	if err := write(out, started, rep, runErr); err != nil {
		return err
	}
	return runErr
}

func median(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	y := append([]float64(nil), x...)
	sort.Float64s(y)
	if len(y)%2 == 1 {
		return y[len(y)/2]
	}
	return (y[len(y)/2-1] + y[len(y)/2]) / 2
}
func minmax(x []float64) (lo, hi float64) {
	if len(x) == 0 {
		return
	}
	lo, hi = x[0], x[0]
	for _, v := range x {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return
}

var variants = []string{rlsbench.Baseline, rlsbench.IdentityOnly, rlsbench.PoliciesOn}

func ratios(reps []rlsbench.Rep) map[string][]float64 {
	r := map[string][]float64{}
	for _, rep := range reps {
		b, i, o := rep.Variants[rlsbench.Baseline], rep.Variants[rlsbench.IdentityOnly], rep.Variants[rlsbench.PoliciesOn]
		if b.Total.OpsPerS == 0 || i.Total.OpsPerS == 0 {
			continue
		}
		r["identity_only / baseline"] = append(r["identity_only / baseline"], i.Total.OpsPerS/b.Total.OpsPerS)
		r["policies_on / identity_only"] = append(r["policies_on / identity_only"], o.Total.OpsPerS/i.Total.OpsPerS)
		r["policies_on / baseline"] = append(r["policies_on / baseline"], o.Total.OpsPerS/b.Total.OpsPerS)
	}
	return r
}

// summarize reduces the repetitions: for each variant, the median of the per-repetition
// throughput and latencies, with the range.
func summarize(reps []rlsbench.Rep) map[string]any {
	out := map[string]any{}
	for _, v := range variants {
		var ops, p50, p99 []float64
		byOp := map[string][2][]float64{}
		for _, rep := range reps {
			r, ok := rep.Variants[v]
			if !ok {
				continue
			}
			ops = append(ops, r.Total.OpsPerS)
			p50 = append(p50, r.Total.P50Ms)
			p99 = append(p99, r.Total.P99Ms)
			for op, st := range r.ByOp {
				e := byOp[op]
				e[0], e[1] = append(e[0], st.P50Ms), append(e[1], st.P99Ms)
				byOp[op] = e
			}
		}
		lo, hi := minmax(ops)
		ops2 := map[string]any{}
		for op, e := range byOp {
			ops2[op] = map[string]float64{"p50_ms_median": median(e[0]), "p99_ms_median": median(e[1])}
		}
		out[v] = map[string]any{
			"ops_per_s_median": median(ops), "ops_per_s_min": lo, "ops_per_s_max": hi,
			"p50_ms_median": median(p50), "p99_ms_median": median(p99), "by_op": ops2,
		}
	}
	return out
}

func write(dir string, started time.Time, rep report, runErr error) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := filepath.Join(dir, "rls-"+started.Format("20060102T150405Z"))
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(base+".json", b, 0o644); err != nil {
		return err
	}
	var s strings.Builder
	e := rep.Env
	fmt.Fprintf(&s, "Row-level security cost benchmark (synthetic data; Postgres in Docker on the same machine as the workers)\n")
	fmt.Fprintf(&s, "started %s, commit %s (working tree dirty: %v)\n", started.Format(time.RFC3339), e.Commit, e.Dirty)
	fmt.Fprintf(&s, "machine: %s, %d CPUs, %s bytes RAM, %s; %s\n", e.Machine, e.CPUs, e.MemoryBytes, e.OS, e.Postgres)
	fmt.Fprintf(&s, "docker: %s; engine %s\nload average at start: %s\n", e.Docker, e.DockerInfo, e.LoadAvg)
	fmt.Fprintf(&s, "parameters: %v\n\n", rep.Params)
	if len(rep.Reps) > 0 {
		fmt.Fprintf(&s, "seeded per repetition: %v\n", rep.Reps[0].Seed)
		fmt.Fprintf(&s, "answers compared across the variants in each repetition: %d (they matched, or the run would have stopped)\n\n", rep.Reps[0].ChecksCompared)
	}
	fmt.Fprintf(&s, "%-14s %12s %22s %12s %12s\n", "variant", "ops/s (med)", "[min - max over reps]", "p50 ms (med)", "p99 ms (med)")
	for _, v := range variants {
		m, ok := rep.Summary[v].(map[string]any)
		if !ok {
			continue
		}
		fmt.Fprintf(&s, "%-14s %12.0f %22s %12.2f %12.2f\n", v, m["ops_per_s_median"], fmt.Sprintf("[%.0f - %.0f]", m["ops_per_s_min"], m["ops_per_s_max"]), m["p50_ms_median"], m["p99_ms_median"])
	}
	fmt.Fprintf(&s, "\nthroughput ratios, per repetition (median [min - max]):\n")
	for _, k := range []string{"identity_only / baseline", "policies_on / identity_only", "policies_on / baseline"} {
		r := rep.Ratios[k]
		lo, hi := minmax(r)
		fmt.Fprintf(&s, "  %-30s %.3f [%.3f - %.3f]  (n=%d)\n", k, median(r), lo, hi, len(r))
	}
	fmt.Fprintf(&s, "\nper operation, median over repetitions of p50 / p99 in ms:\n%-15s", "")
	for _, v := range variants {
		fmt.Fprintf(&s, " %22s", v)
	}
	fmt.Fprintln(&s)
	for _, op := range []string{"list_groups", "list_expenses", "get_expense", "balances", "claim_unclaim"} {
		fmt.Fprintf(&s, "%-15s", op)
		for _, v := range variants {
			m, _ := rep.Summary[v].(map[string]any)
			by, _ := m["by_op"].(map[string]any)
			o, _ := by[op].(map[string]float64)
			fmt.Fprintf(&s, " %10.2f / %-9.2f", o["p50_ms_median"], o["p99_ms_median"])
		}
		fmt.Fprintln(&s)
	}
	if len(rep.Reps) > 0 && len(rep.Reps[0].Plans) > 0 {
		fmt.Fprintf(&s, "\nplans with the policies on (repetition 1), run as one user:\n")
		keys := make([]string, 0)
		for k := range rep.Reps[0].Plans {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&s, "--- %s\n%s", k, rep.Reps[0].Plans[k])
		}
	}
	if runErr != nil {
		fmt.Fprintf(&s, "\nRUN FAILED: %v\n", runErr)
	}
	if err := os.WriteFile(base+".txt", []byte(s.String()), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s.json and .txt\n", base)
	return nil
}
