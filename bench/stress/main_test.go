package main

import (
	"strings"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/benchwork"
	"github.com/riyne/fifty-fifty/internal/stressrig"
)

// The summary is what a reader sees of a recording, so it must say what was disabled,
// list zero violations as well as non-zero ones, show the layer evidence, and mark a run
// that did not match its expectation.
func TestRender(t *testing.T) {
	counts := map[string]int64{
		"http_5xx": 3, stressrig.LogErrorPrefix + "client_gone": 40,
		stressrig.LogErrorPrefix + "unique:ledger_transactions_one_void": 3,
		stressrig.WorkerLogErrorPrefix + "other:job failed: boom":        2,
	}
	rep := report{
		StartedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Build: "controls",
		Params: params{Runs: 1, Latency: "100ms", Duration: "1m0s"},
		Env:    benchwork.Env{Commit: "abc"},
		Configs: []configReport{{Name: "void_lock", Controls: []string{"void_lock"}, Runs: []runReport{
			{Seed: 1, Clean: true, Result: stressrig.Result{Counts: counts, Violations: map[stressrig.Invariant]int{}}},
			{Seed: 2, Clean: false, Problems: []string{"a layer was not seen"},
				Result: stressrig.Result{Counts: counts, Violations: map[stressrig.Invariant]int{stressrig.DuplicateKey: 7}}},
		}}},
	}
	out := render(rep)
	for _, want := range []string{
		"controls build", "mock payment provider with simulated latency of 100ms", "protections disabled: [void_lock]",
		"run seed 1: matches expectation", "run seed 2: DOES NOT MATCH: a layer was not seen",
		"invariants violated: none", "duplicate_per_idempotency_key=7",
		"ledger_transaction_not_zero=[0 0]", "duplicate_per_idempotency_key=[0 7]", // zeros are listed, in seed order
		"api logged: 40 from clients going away; other errors:", "unique:ledger_transactions_one_void = 3",
		"workers logged: 0 from clients going away; other errors:", "other:job failed: boom = 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render is missing %q\n%s", want, out)
		}
	}
}
