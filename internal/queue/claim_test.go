package queue_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// Claim tests share the database with every other test, so each one works on a
// kind of its own and claims only that kind; ClaimJobs' kind filter is what keeps
// them from stealing each other's jobs (and from touching the leftovers of
// earlier runs).
func newKind(prefix string) string { return "test." + prefix + "." + uuid.NewString() }

// enqueue commits one job and returns its id.
func enqueue(t *testing.T, pool *pgxpool.Pool, j queue.NewJob) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	inTx(t, pool, true, func(tx pgx.Tx) {
		var err error
		if id, err = queue.Enqueue(ctx, tx, j); err != nil {
			t.Fatal(err)
		}
	})
	return id
}

func claim(t *testing.T, pool *pgxpool.Pool, kind string, limit int) []queue.Claimed {
	t.Helper()
	got, err := queue.ClaimJobs(ctx, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: limit, Lease: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func ids(cs []queue.Claimed) []uuid.UUID {
	out := make([]uuid.UUID, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

type pgtypeUUID = pgtype.UUID

type jobRow struct {
	State             string
	Attempts          int
	MaxAttempts       int
	Token             *uuid.UUID
	LeaseExpiresAt    *time.Time
	RunAt             time.Time
	FinishedAt        *time.Time
	LastError         *string
	UntilLeaseExpires *time.Duration // lease_expires_at - now(), by the database's clock
}

func getJob(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) jobRow {
	t.Helper()
	var r jobRow
	var tok pgtypeUUID
	err := pool.QueryRow(ctx, `
		SELECT state, attempts, max_attempts, lease_token, lease_expires_at, run_at, finished_at, last_error,
		       lease_expires_at - now()
		FROM jobs WHERE id = $1`, id).
		Scan(&r.State, &r.Attempts, &r.MaxAttempts, &tok, &r.LeaseExpiresAt, &r.RunAt, &r.FinishedAt, &r.LastError, &r.UntilLeaseExpires)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Valid {
		u := uuid.UUID(tok.Bytes)
		r.Token = &u
	}
	return r
}

// expireLease makes a running job's lease run out now, the way time passing
// would, keeping run_at equal to the lease expiry as the table requires. Tests
// do this instead of sleeping.
func expireLease(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	tag, err := pool.Exec(ctx, `
		UPDATE jobs SET run_at = now() - interval '1 second', lease_expires_at = now() - interval '1 second'
		WHERE id = $1 AND state = 'running'`, id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("expire lease: %v, %d rows", err, tag.RowsAffected())
	}
}

func past(d time.Duration) time.Time { return time.Now().Add(-d) }

func TestClaim_LeasesADueJob(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("lease")
	id := enqueue(t, pool, queue.NewJob{Kind: kind, Payload: map[string]any{"n": 1}, MaxAttempts: 3})

	got := claim(t, pool, kind, 5)
	if len(got) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(got))
	}
	c := got[0]
	if c.ID != id || c.Kind != kind || c.Attempt != 1 || c.MaxAttempts != 3 || string(c.Payload) != `{"n": 1}` {
		t.Errorf("claimed = %+v", c)
	}

	row := getJob(t, pool, id)
	if row.State != queue.StateRunning || row.Attempts != 1 {
		t.Errorf("row = %+v, want running on attempt 1", row)
	}
	if row.Token == nil || *row.Token != c.LeaseToken || c.LeaseToken == uuid.Nil {
		t.Errorf("stored token %v does not match the claimed one %v", row.Token, c.LeaseToken)
	}
	// 30 s lease on the database clock; the bounds only allow for the test's own latency.
	if d := *row.UntilLeaseExpires; d < 25*time.Second || d > 30*time.Second {
		t.Errorf("lease expires in %v, want about 30s", d)
	}
	if !row.RunAt.Equal(*row.LeaseExpiresAt) || !c.LeaseExpiresAt.Equal(*row.LeaseExpiresAt) {
		t.Errorf("run_at %v, lease_expires_at %v and the claimed expiry %v must all agree", row.RunAt, row.LeaseExpiresAt, c.LeaseExpiresAt)
	}
	// Holding a lease makes the job invisible to other claimers.
	if again := claim(t, pool, kind, 5); len(again) != 0 {
		t.Errorf("a job with a live lease was claimed again: %+v", again)
	}
}

func TestClaim_SkipsIneligibleJobs(t *testing.T) {
	pool := testutil.Pool(t)
	kind, otherKind := newKind("skip"), newKind("skip-other")

	enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: time.Now().Add(time.Hour)}) // not due yet
	enqueue(t, pool, queue.NewJob{Kind: otherKind, RunAt: past(time.Minute)})    // due, but not our kind
	done := enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: past(time.Minute)}) // finished
	dead := enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: past(time.Minute)}) // dead-lettered
	exhausted := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1, RunAt: past(time.Minute)})
	for id, q := range map[uuid.UUID]string{
		done: `UPDATE jobs SET state = 'succeeded', attempts = 1, finished_at = now() WHERE id = $1`,
		dead: `UPDATE jobs SET state = 'dead', attempts = 1, finished_at = now() WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	// Used up its only attempt and the lease has since expired: not claimable again.
	if c := claim(t, pool, kind, 10); len(c) != 1 || c[0].ID != exhausted {
		t.Fatalf("setup: expected to claim only the single-attempt job, got %+v", c)
	}
	expireLease(t, pool, exhausted)

	// One eligible job among all of that proves the claim works and filters.
	want := enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: past(time.Minute)})
	got := claim(t, pool, kind, 10)
	if len(got) != 1 || got[0].ID != want {
		t.Fatalf("claimed %v, want only %v", ids(got), want)
	}
}

func TestClaim_TakesOverAnExpiredLease(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("takeover")
	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})

	first := claim(t, pool, kind, 1)
	if len(first) != 1 {
		t.Fatal("first claim got nothing")
	}
	expireLease(t, pool, id) // the first worker has gone silent

	second := claim(t, pool, kind, 1)
	if len(second) != 1 || second[0].ID != id {
		t.Fatalf("expired lease not taken over: %+v", second)
	}
	if second[0].Attempt != 2 {
		t.Errorf("attempt = %d, want 2", second[0].Attempt)
	}
	if second[0].LeaseToken == first[0].LeaseToken {
		t.Error("takeover reused the old lease token, so the old worker would not be fenced out")
	}
	if row := getJob(t, pool, id); row.State != queue.StateRunning || *row.Token != second[0].LeaseToken || row.Attempts != 2 {
		t.Errorf("row after takeover = %+v", row)
	}
}

func TestClaim_OldestFirstAndLimited(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("order")
	// Enqueued out of order on purpose.
	age := []time.Duration{3, 5, 1, 4, 2}
	byAge := map[time.Duration]uuid.UUID{}
	for _, a := range age {
		byAge[a] = enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: past(a * time.Minute)})
	}

	first := claim(t, pool, kind, 2)
	if want := []uuid.UUID{byAge[5], byAge[4]}; !slices.Equal(sorted(ids(first)), sorted(want)) || len(first) != 2 {
		t.Fatalf("limit 2 claimed %v, want the two oldest %v", ids(first), want)
	}
	rest := claim(t, pool, kind, 10)
	if len(rest) != 3 {
		t.Fatalf("the other three were not left for the next claim: %d", len(rest))
	}
}

func sorted(in []uuid.UUID) []uuid.UUID {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// SKIP LOCKED, shown deterministically: a transaction stands in for another
// worker that is in the middle of claiming a row. Racing real workers would only
// hit this by luck.
func TestClaim_PassesOverRowsLockedByAnotherClaimer(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("locked")
	a := enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: past(2 * time.Minute)}) // oldest, so it would be picked first
	b := enqueue(t, pool, queue.NewJob{Kind: kind, RunAt: past(time.Minute)})

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	lock := func(id uuid.UUID) {
		if _, err := holder.Exec(ctx, `SELECT 1 FROM jobs WHERE id = $1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
	}

	// A claim that waited for the lock would sit here until the context expires.
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	claimBounded := func(limit int) []queue.Claimed {
		got, err := queue.ClaimJobs(bounded, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: limit, Lease: 30 * time.Second})
		if err != nil {
			t.Fatalf("claim while a row is locked: %v (it must skip, not wait)", err)
		}
		return got
	}

	lock(a)
	if got := claimBounded(2); len(got) != 1 || got[0].ID != b {
		t.Fatalf("with the oldest job locked, claimed %v, want only %v", ids(got), b)
	}
	lock(b) // b is running now, but lock it too: nothing claimable and nothing unlocked
	if got := claimBounded(2); len(got) != 0 {
		t.Fatalf("claimed %v while every row was locked or leased", ids(got))
	}

	holder.Rollback(ctx) //nolint:errcheck // release a
	if got := claim(t, pool, kind, 2); len(got) != 1 || got[0].ID != a {
		t.Fatalf("after the lock was released, claimed %v, want %v", ids(got), a)
	}
}

// Randomized complement to the test above: many workers draining one kind. Every
// job must be handed out exactly once.
func TestClaim_ConcurrentClaimersNeverShareAJob(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("race")
	const jobs, workers, batch = 300, 8, 7

	var want []uuid.UUID
	inTx(t, pool, true, func(tx pgx.Tx) {
		for range jobs {
			id, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: kind})
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, id)
		}
	})

	var mu sync.Mutex
	claimedBy := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			for {
				got, err := queue.ClaimJobs(ctx, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: batch, Lease: time.Minute})
				if err != nil {
					errs <- err
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, c := range got {
					claimedBy[c.ID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	for _, id := range want {
		if n := claimedBy[id]; n != 1 {
			t.Errorf("job %v claimed %d times, want 1", id, n)
		}
	}
	if len(claimedBy) != jobs {
		t.Errorf("%d distinct jobs claimed, want %d", len(claimedBy), jobs)
	}
	var running, attemptsOK int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'running'), count(*) FILTER (WHERE attempts = 1) FROM jobs WHERE kind = $1`, kind).Scan(&running, &attemptsOK); err != nil {
		t.Fatal(err)
	}
	if running != jobs || attemptsOK != jobs {
		t.Errorf("table says %d running, %d on attempt 1; want %d of each", running, attemptsOK, jobs)
	}
}

func TestClaim_RejectsBadParams(t *testing.T) {
	pool := testutil.Pool(t)
	ok := queue.ClaimParams{Kinds: []string{"k"}, Limit: 1, Lease: time.Second}
	for name, mutate := range map[string]func(p *queue.ClaimParams){
		"no kinds":       func(p *queue.ClaimParams) { p.Kinds = nil },
		"limit zero":     func(p *queue.ClaimParams) { p.Limit = 0 },
		"limit too big":  func(p *queue.ClaimParams) { p.Limit = queue.MaxClaimBatch + 1 },
		"lease zero":     func(p *queue.ClaimParams) { p.Lease = 0 },
		"lease negative": func(p *queue.ClaimParams) { p.Lease = -time.Second },
	} {
		p := ok
		mutate(&p)
		if _, err := queue.ClaimJobs(ctx, pool, p); !errors.Is(err, queue.ErrInvalidClaim) {
			t.Errorf("%s: err = %v, want ErrInvalidClaim", name, err)
		}
	}
}

// A worker that dies on the last attempt cannot report anything. The job must
// not be claimable again (that would be attempt max+1) and must not stay
// "running" forever.
func TestReapExhausted(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("reap")
	lastAttempt := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1})
	stillLeased := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1})
	hasAttemptsLeft := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 2})
	expiredWithAttemptsLeft := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 2})

	if got := claim(t, pool, kind, 10); len(got) != 4 {
		t.Fatalf("setup: claimed %d, want 4", len(got))
	}
	expireLease(t, pool, lastAttempt)
	expireLease(t, pool, hasAttemptsLeft)
	expireLease(t, pool, expiredWithAttemptsLeft)

	// Claimed before reaping, so this is the claim's own refusal of the exhausted job.
	// It re-leases hasAttemptsLeft; expiredWithAttemptsLeft stays expired for the
	// reaper to leave alone.
	got := claim(t, pool, kind, 1)
	if len(got) != 1 || got[0].ID == lastAttempt {
		t.Fatalf("claimed %v; the exhausted job must not be claimable", ids(got))
	}
	reclaimed := got[0].ID
	if reclaimed != hasAttemptsLeft && reclaimed != expiredWithAttemptsLeft {
		t.Fatalf("claimed %v, want one of the expired jobs with an attempt left", reclaimed)
	}
	// Both have the same age, so which one is claimed first is not specified. Whichever
	// it was, the other is the one still expired for the reaper.
	stillExpired := hasAttemptsLeft
	if reclaimed == hasAttemptsLeft {
		stillExpired = expiredWithAttemptsLeft
	}

	reaped, err := queue.ReapExhausted(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(reaped, lastAttempt) || slices.Contains(reaped, stillLeased) ||
		slices.Contains(reaped, reclaimed) || slices.Contains(reaped, stillExpired) {
		t.Errorf("reaped %v: want the last-attempt job only", reaped)
	}
	if row := getJob(t, pool, stillExpired); row.State != queue.StateRunning || row.Attempts != 1 {
		t.Errorf("an expired job with an attempt left must be left for a worker to claim, got %+v", row)
	}

	row := getJob(t, pool, lastAttempt)
	if row.State != queue.StateDead || row.FinishedAt == nil || row.Token != nil || row.LeaseExpiresAt != nil {
		t.Errorf("reaped row = %+v, want dead, finished, no lease", row)
	}
	if row.LastError == nil || !strings.Contains(*row.LastError, "lease expired on attempt 1 of 1") {
		t.Errorf("last_error = %v", row.LastError)
	}
	if row := getJob(t, pool, stillLeased); row.State != queue.StateRunning {
		t.Errorf("a job with a live lease was reaped: %+v", row)
	}
	if row := getJob(t, pool, reclaimed); row.State != queue.StateRunning || row.Attempts != 2 {
		t.Errorf("a re-leased job was touched: %+v", row)
	}
	// The reaped job is never handed out again; the one still expired is.
	if got := claim(t, pool, kind, 10); len(got) != 1 || got[0].ID != stillExpired {
		t.Errorf("claimed %v, want only %v", ids(got), stillExpired)
	}
}
