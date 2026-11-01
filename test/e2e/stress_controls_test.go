//go:build unsafe_controls

package e2e_test

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/riyne/fifty-fifty/internal/settlerig"
	"github.com/riyne/fifty-fifty/internal/stressrig"
)

// These are the negative controls at smoke size (make test-controls): the stress workload
// again, with one protection switched off, in the api and in the workers, which are built
// with the unsafe_controls tag. Each case says what the harness must see:
//
//   - violated: the invariants that must be violated (and nothing else may be), or none;
//   - layers: errors the api must have logged, each a database layer or a guard that
//     stopped the problem. A control with no violation and a layer is "caught by that
//     layer"; one with a violation is "nothing caught it".
//
// Whatever the case, the three zero-sum invariants must hold: the database enforces them.
// Settlement is simulated: the provider is a mock with simulated latency of 100 ms.

var (
	unsafeBinsOnce sync.Once
	unsafeBins     settlerig.Bins
	unsafeBinsErr  error
)

func tagged(t *testing.T) settlerig.Bins {
	t.Helper()
	unsafeBinsOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fifty-unsafe-bin-")
		if err != nil {
			unsafeBinsErr = err
			return
		}
		unsafeBins, unsafeBinsErr = settlerig.BuildWithTags(ctx, dir, "unsafe_controls")
	})
	if unsafeBinsErr != nil {
		t.Fatal(unsafeBinsErr)
	}
	return unsafeBins
}

func TestStressControls(t *testing.T) {
	if !enabled {
		t.Skip("set E2E=1")
	}
	const (
		postingIdx = "unique:ledger_transactions_one_settlement_posting"
		onePosting = "unique:ledger_transactions_one_expense_posting"
		oneVoid    = "unique:ledger_transactions_one_void"
	)
	cases := []struct {
		name      string
		off       []string
		dropIndex bool
		resolve   bool // use the mock that makes resolvers meet
		violated  []stressrig.Invariant
		layers    []string // substrings; each must match a logged error class
		silent    bool     // no error may be logged at all (apart from clients going away)
	}{
		{name: "claim_lock", off: []string{"claim_lock"}, violated: []stressrig.Invariant{stressrig.SharesMatchClaims}},
		{name: "finalize_lock", off: []string{"finalize_lock"}, violated: []stressrig.Invariant{stressrig.SharesMatchClaims}, layers: []string{onePosting}},
		{name: "void_lock", off: []string{"void_lock"}, layers: []string{oneVoid}},
		{name: "idempotency_check", off: []string{"idempotency_check"}, violated: []stressrig.Invariant{stressrig.DuplicateKey}},
		{name: "group_lock", off: []string{"group_lock"}, violated: []stressrig.Invariant{stressrig.OverCapacity}},
		// The Resolve ladder, in the order the success path meets the layers: the row lock,
		// then the unique index on the posting (the posting is inserted first), then the
		// guard on the status update.
		{name: "resolve_guards (lock still on)", off: []string{"resolve_guards"}, resolve: true, silent: true},
		{name: "resolve_lock", off: []string{"resolve_lock"}, resolve: true, layers: []string{postingIdx}},
		{name: "resolve_lock + posting index", off: []string{"resolve_lock"}, dropIndex: true, resolve: true, layers: []string{"mark succeeded: settlement was not in a state"}},
		{name: "resolve_lock + posting index + guards", off: []string{"resolve_lock", "resolve_guards"}, dropIndex: true, resolve: true,
			violated: []stressrig.Invariant{stressrig.SettlementPosting, stressrig.ProviderBooks}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := stressParams(t)
			p.Bins = tagged(t)
			p.Controls, p.DropPostingIndex = c.off, c.dropIndex
			if c.resolve {
				p.MockFlags = stressrig.ResolveRaceMockFlags
			}
			res, err := stressrig.Run(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("controls off: %v, posting index dropped: %v\n%s", c.off, c.dropIndex, res.Summary())

			for _, inv := range []stressrig.Invariant{stressrig.ZeroSumTransaction, stressrig.ZeroSumGroup, stressrig.ZeroSumLedger} {
				if n := res.Violations[inv]; n != 0 {
					t.Errorf("%s violated %d times: the database's own zero-sum rule must hold under every control", inv, n)
				}
			}
			want := map[stressrig.Invariant]bool{}
			for _, inv := range c.violated {
				want[inv] = true
				if res.Violations[inv] == 0 {
					t.Errorf("%s was not violated: the harness did not see the effect of disabling %v", inv, c.off)
				}
			}
			for inv, n := range res.Violations {
				if n > 0 && !want[inv] {
					t.Errorf("%s violated %d times, which this control should not cause", inv, n)
				}
			}
			for _, layer := range c.layers {
				found := false
				for k, n := range res.Counts {
					if n > 0 && strings.HasPrefix(k, stressrig.LogErrorPrefix) && strings.Contains(k, layer) {
						found = true
					}
				}
				if !found {
					t.Errorf("no logged error matching %q: the layer that should have caught it was not seen", layer)
				}
			}
			if c.silent && len(res.ServerErrors()) > 0 {
				t.Errorf("errors logged that should not occur: %v", res.ServerErrors())
			}
			if res.Unresolved > 0 {
				t.Errorf("%d settlements never resolved", res.Unresolved)
			}
			for _, v := range res.Vacuous(p) {
				t.Errorf("vacuous: %s", v)
			}
		})
	}
}
