package rlsbench_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/rlsbench"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

func params(t *testing.T) rlsbench.Params {
	t.Helper()
	testutil.Pool(t) // skip, or fail under REQUIRE_DATABASE=1, with no database
	owner, app := os.Getenv("TEST_OWNER_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL")
	if owner == "" || app == "" {
		t.Skip("TEST_OWNER_DATABASE_URL and TEST_DATABASE_URL are required")
	}
	return rlsbench.Params{
		OwnerURL: owner, AppURL: app,
		Groups: 8, MembersPerGroup: 4, ExpensesPerGroup: 10,
		Workers: 4, Warmup: 200 * time.Millisecond, Measure: 600 * time.Millisecond,
		Reps: 2, Seed: 1,
	}
}

// A small run on a scratch database: every variant completes without a failed operation,
// the three variants give identical answers to the fixed questions (Run fails otherwise),
// row-level security is off in the first two variants and on in the third, and an
// unguarded count sees the whole table in the first two and only the user's groups in the
// third (Run fails if the switch did not take effect).
func TestRunMeasuresThreeVariantsThatAgree(t *testing.T) {
	reps, err := rlsbench.Run(context.Background(), params(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 2 {
		t.Fatalf("%d repetitions, want 2", len(reps))
	}
	seenOrders := map[string]bool{}
	for _, rep := range reps {
		seenOrders[rep.Order[0]] = true
		if rep.ChecksCompared < 100 {
			t.Errorf("rep %d compared only %d answers", rep.Rep, rep.ChecksCompared)
		}
		for _, v := range []string{rlsbench.Baseline, rlsbench.IdentityOnly, rlsbench.PoliciesOn} {
			r, ok := rep.Variants[v]
			if !ok {
				t.Fatalf("rep %d: variant %s missing", rep.Rep, v)
			}
			if r.Total.N == 0 || r.Total.Failures != 0 || r.Total.OpsPerS <= 0 {
				t.Errorf("rep %d %s: %+v", rep.Rep, v, r.Total)
			}
			for op, st := range r.ByOp {
				if st.N == 0 {
					t.Errorf("rep %d %s: operation %s never ran", rep.Rep, v, op)
				}
			}
			wantRLS := 0
			if v == rlsbench.PoliciesOn {
				wantRLS = 14
			}
			if r.TablesWithRLS != wantRLS {
				t.Errorf("rep %d %s: %d tables with RLS, want %d", rep.Rep, v, r.TablesWithRLS, wantRLS)
			}
			if v == rlsbench.PoliciesOn && r.UnguardedCount >= rep.OwnerExpenses {
				t.Errorf("rep %d: with policies on an unguarded count saw %d of %d expenses", rep.Rep, r.UnguardedCount, rep.OwnerExpenses)
			}
			if v != rlsbench.PoliciesOn && r.UnguardedCount != rep.OwnerExpenses {
				t.Errorf("rep %d %s: an unguarded count saw %d of %d expenses, want all", rep.Rep, v, r.UnguardedCount, rep.OwnerExpenses)
			}
		}
	}
	if len(seenOrders) < 2 {
		t.Errorf("the variant order did not rotate: %v", seenOrders)
	}
	// The plans were captured and show a subplan, not a function call per row.
	if len(reps[0].Plans) != 3 {
		t.Fatalf("%d plans captured, want 3", len(reps[0].Plans))
	}
}
