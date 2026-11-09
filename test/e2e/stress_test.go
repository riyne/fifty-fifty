package e2e_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/harness/stressrig"
)

// stressParams is a small, fast version of the run bench/stress will record. Settlement
// is simulated: the provider is a mock with simulated latency of 100 ms.
func stressParams(t *testing.T) stressrig.Params {
	t.Helper()
	owner, app := os.Getenv("MIGRATE_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if owner == "" {
		owner = os.Getenv("TEST_OWNER_DATABASE_URL")
	}
	if app == "" {
		app = os.Getenv("TEST_DATABASE_URL")
	}
	if owner == "" || app == "" {
		t.Skip("needs TEST_OWNER_DATABASE_URL and TEST_DATABASE_URL")
	}
	return stressrig.Params{
		OwnerURL: owner, AppURL: app, Bins: bins,
		Seed: 1, Duration: 20 * time.Second,
		Users: 12, Groups: 2, RaceLoops: 2,
		SettleWorkers: 2, RoundPause: 300 * time.Millisecond,
		Workers: 3, WorkerConcurrency: 4, Lease: 5 * time.Second, Poll: time.Second,
		MockFlags: stressrig.DefaultMockFlags, TimeoutHold: 3 * time.Second, WebhookSecret: webhookSecret,
		ResolveTimeout: 3 * time.Minute,
		Logf:           t.Logf,
	}
}

// TestStress_ProductionBuildKeepsEveryInvariant runs the whole workload (expenses,
// claims, finalizes, voids, settlements, retries, duplicate keys, abandoned requests and
// barrier races) against the production build and requires every invariant to hold. It
// also requires that the dangerous things happened: a run that never raced proves nothing.
func TestStress_ProductionBuildKeepsEveryInvariant(t *testing.T) {
	if !enabled {
		t.Skip("set E2E=1 (make test-e2e)")
	}
	p := stressParams(t)
	res, err := stressrig.Run(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, k := range stressrig.Keys(res.Counts) {
		fmt.Fprintf(&b, "%s=%d  ", k, res.Counts[k])
	}
	t.Logf("counts: %s", b.String())
	t.Logf("lock waits: %+v; checked: %+v; unresolved settlements: %d; elapsed %.0fs", res.Locks, res.Checked, res.Unresolved, res.Elapsed)
	if res.Provider != nil {
		t.Logf("provider: %+v", *res.Provider)
	}
	if !res.Report.OK() {
		t.Errorf("invariants violated:\n%s", res.Report)
	}
	if len(res.Unexpected) > 0 || res.Counts["http_5xx"] > 0 {
		t.Errorf("unexpected responses (%d, 5xx %d): %v", len(res.Unexpected), res.Counts["http_5xx"], res.Unexpected)
	}
	if se := res.ServerErrors(); len(se) > 0 {
		t.Errorf("the api logged errors that are not a client going away: %v", se)
	}
	if res.Unresolved > 0 {
		t.Errorf("%d settlements never reached a final state", res.Unresolved)
	}
	for _, v := range res.Vacuous(p) {
		t.Errorf("vacuous: %s", v)
	}
}
