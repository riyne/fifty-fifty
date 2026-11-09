package settlements_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/harness/mockpay"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/settlements"
)

// A job as the watchdog sees it, inserted directly: the states a real job reaches by
// running (and dying) take real time, and the watchdog only reads the table.
func (r *rig) addExecuteJob(t *testing.T, id uuid.UUID, state string, age time.Duration) {
	t.Helper()
	cols := map[string]string{
		"queued":  `state, max_attempts`,
		"running": `state, max_attempts, attempts, lease_token, lease_expires_at, run_at`,
		"dead":    `state, max_attempts, attempts, finished_at, last_error`,
	}[state]
	vals := map[string]string{
		"queued":  `'queued', 5`,
		"running": `'running', 5, 1, gen_random_uuid(), now() + interval '1 hour', now() + interval '1 hour'`,
		"dead":    `'dead', 5, 5, now(), 'earlier failure'`,
	}[state]
	_, err := r.owner.Exec(ctx, fmt.Sprintf(
		`INSERT INTO jobs (kind, payload, created_at, %s) VALUES ('settlement.execute', jsonb_build_object('settlement_id', $1::text), now() - make_interval(secs => $2::float8), %s)`,
		cols, vals), id.String(), age.Seconds())
	if err != nil {
		t.Fatal(err)
	}
}

// oldSettlement is a pending settlement created long enough ago for the watchdog to care.
func (r *rig) oldSettlement(t *testing.T, amount int64) uuid.UUID {
	t.Helper()
	id := r.settlement(t, amount)
	if _, err := r.owner.Exec(ctx, `UPDATE settlements SET created_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (r *rig) countQuery(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := r.owner.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *rig) executeJobs(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := r.owner.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'settlement.execute' AND payload->>'settlement_id' = $1`, id.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sweep runs the watchdog handler once, on this rig's group only.
func (r *rig) sweep(t *testing.T, cfg settlements.WatchdogConfig) {
	t.Helper()
	g := r.group
	cfg.Group = &g
	if cfg.StuckAfter == 0 {
		cfg.StuckAfter = time.Minute
	}
	commit, err := settlements.WatchdogHandler(r.pool, cfg)(ctx, queue.Claimed{})
	if err != nil || commit != nil {
		t.Fatalf("watchdog: commit=%v err=%v", commit != nil, err)
	}
}

func TestWatchdog_ReplacesALostExecuteJob(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	withDead := r.oldSettlement(t, 100)
	r.addExecuteJob(t, withDead, "dead", time.Hour)
	noJob := r.oldSettlement(t, 200) // its job never existed (or was deleted)

	r.sweep(t, settlements.WatchdogConfig{})
	if r.executeJobs(t, withDead) != 2 || r.executeJobs(t, noJob) != 1 {
		t.Fatalf("execute jobs: %d and %d, want 2 and 1", r.executeJobs(t, withDead), r.executeJobs(t, noJob))
	}
	var state string
	if err := r.owner.QueryRow(ctx, `SELECT state FROM jobs WHERE kind = 'settlement.execute' AND payload->>'settlement_id' = $1 AND state <> 'dead'`, withDead.String()).Scan(&state); err != nil || state != "queued" {
		t.Errorf("the replacement is %q (%v), want queued", state, err)
	}
	if n := r.countQuery(t, `SELECT count(*) FROM audit_events WHERE group_id = $1 AND kind = 'settlement.execute_requeued' AND data->>'settlement_id' = $2 AND (data->>'earlier_execute_jobs')::int = 1`, r.group, withDead.String()); n != 1 {
		t.Errorf("requeue audit events = %d, want 1 (recording the earlier job)", n)
	}

	// Sweeping again at once changes nothing: there is a live job now.
	r.sweep(t, settlements.WatchdogConfig{})
	if r.executeJobs(t, withDead) != 2 || r.executeJobs(t, noJob) != 1 {
		t.Error("a second sweep enqueued again")
	}
}

func TestWatchdog_LeavesWhatIsNotStuckAlone(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	cases := map[string]uuid.UUID{}
	add := func(name string, id uuid.UUID) { cases[name] = id }

	live := r.oldSettlement(t, 1)
	r.addExecuteJob(t, live, "queued", 2*time.Hour) // old, but still waiting its turn
	add("a queued execute job, however old", live)

	running := r.oldSettlement(t, 2)
	r.addExecuteJob(t, running, "running", 2*time.Hour)
	add("a running execute job", running)

	recent := r.oldSettlement(t, 3)
	r.addExecuteJob(t, recent, "dead", 10*time.Second) // died a moment ago: give it time before replacing
	add("a job that died moments ago", recent)

	capped := r.oldSettlement(t, 4)
	for range 3 {
		r.addExecuteJob(t, capped, "dead", time.Hour)
	}
	add("three dead jobs already (the cap)", capped)

	young := r.settlement(t, 5) // created just now, no job yet
	add("a settlement created moments ago", young)

	for _, state := range []string{"unknown", "succeeded", "failed"} {
		id := r.oldSettlement(t, 6)
		switch state {
		case "unknown":
			r.resolve(t, id, settlements.AmbiguousResult())
		case "succeeded":
			r.resolve(t, id, settlements.Success())
		case "failed":
			r.resolve(t, id, settlements.Failure("declined"))
		}
		add("a "+state+" settlement", id)
	}

	before := map[string]int{}
	for name, id := range cases {
		before[name] = r.executeJobs(t, id)
	}
	r.sweep(t, settlements.WatchdogConfig{})
	for name, id := range cases {
		if got := r.executeJobs(t, id); got != before[name] {
			t.Errorf("%s: execute jobs %d -> %d; the watchdog must leave it alone", name, before[name], got)
		}
	}
}

func TestWatchdog_StopsReplacingAtTheCap(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	two := r.oldSettlement(t, 1)
	r.addExecuteJob(t, two, "dead", time.Hour)
	r.addExecuteJob(t, two, "dead", time.Hour)
	r.sweep(t, settlements.WatchdogConfig{})
	if r.executeJobs(t, two) != 3 {
		t.Fatalf("with 2 dead jobs: %d jobs after the sweep, want 3", r.executeJobs(t, two))
	}
	// The third dies as well (backdate it so it is not "recent") and nothing more is created.
	if _, err := r.owner.Exec(ctx, `UPDATE jobs SET state = 'dead', attempts = 5, finished_at = now(), lease_token = NULL, lease_expires_at = NULL,
	    created_at = now() - interval '1 hour' WHERE kind = 'settlement.execute' AND payload->>'settlement_id' = $1 AND state = 'queued'`, two.String()); err != nil {
		t.Fatal(err)
	}
	r.sweep(t, settlements.WatchdogConfig{})
	if r.executeJobs(t, two) != 3 {
		t.Errorf("%d jobs after the cap was reached, want 3", r.executeJobs(t, two))
	}
	if r.row(t, two).status != "pending" {
		t.Error("the settlement changed")
	}
}

// A watchdog that overlaps another (a slow run, two workers) takes different rows: each
// stuck settlement gets exactly one new job.
func TestWatchdog_OverlappingSweepsEnqueueOncePerSettlement(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	var ids []uuid.UUID
	for i := range 24 {
		ids = append(ids, r.oldSettlement(t, int64(100+i)))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r.sweep(t, settlements.WatchdogConfig{Batch: 5})
		}()
	}
	close(start)
	wg.Wait()
	for _, id := range ids {
		if n := r.executeJobs(t, id); n != 1 {
			t.Errorf("settlement %s has %d execute jobs, want exactly 1", id, n)
		}
	}
}

func TestWatchdog_OneRunDrainsMoreThanOneBatch(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	var ids []uuid.UUID
	for i := range 7 {
		ids = append(ids, r.oldSettlement(t, int64(10+i)))
	}
	r.sweep(t, settlements.WatchdogConfig{Batch: 2})
	for _, id := range ids {
		if r.executeJobs(t, id) != 1 {
			t.Fatalf("settlement %s was not picked up by a run with batches of 2", id)
		}
	}
}

func TestWatchdog_TouchesOnlyTheGroupItWasGiven(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	other := newWorld(t)
	var otherID uuid.UUID
	if err := r.owner.QueryRow(ctx, `INSERT INTO settlements (group_id, from_member, to_member, amount_minor, created_at)
	    VALUES ($1, $2, $3, 100, now() - interval '1 hour') RETURNING id`, other.group, other.from, other.to).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = r.owner.Exec(ctx, `DELETE FROM jobs WHERE payload->>'settlement_id' = $1`, otherID.String())
	})
	mine := r.oldSettlement(t, 100)

	r.sweep(t, settlements.WatchdogConfig{})
	if r.executeJobs(t, mine) != 1 || r.executeJobs(t, otherID) != 0 {
		t.Errorf("jobs: mine %d, other group's %d; want 1 and 0", r.executeJobs(t, mine), r.executeJobs(t, otherID))
	}
}

// The whole point, end to end: an execute job died, the watchdog replaced it, a worker
// ran the replacement, and the settlement is resolved exactly once.
func TestWatchdog_ARecoveredSettlementIsExecutedOnce(t *testing.T) {
	r := newRig(t, mockpay.Config{})
	id := r.oldSettlement(t, 700)
	r.addExecuteJob(t, id, "dead", time.Hour)
	r.sweep(t, settlements.WatchdogConfig{})

	// Hand the replacement to this test's worker: a worker on the real kind would also
	// claim every other test's leftover execute jobs.
	var replacement uuid.UUID
	if err := r.owner.QueryRow(ctx, `UPDATE jobs SET kind = $1 WHERE kind = 'settlement.execute' AND state = 'queued'
	    AND payload->>'settlement_id' = $2 RETURNING id`, r.kind, id.String()).Scan(&replacement); err != nil {
		t.Fatal(err)
	}
	r.run(t, r.executeHandler(r.client))
	if j := r.waitJob(t, replacement, "succeeded", "dead"); j.state != "succeeded" {
		t.Fatalf("replacement = %+v", j)
	}
	if r.row(t, id).status != "succeeded" || r.postings(t, id) != 1 {
		t.Error("the recovered settlement was not posted exactly once")
	}
	if st := r.mock.Stats(); st.Payments != 1 || st.Charges != 1 {
		t.Errorf("provider stats = %+v", st)
	}
}
