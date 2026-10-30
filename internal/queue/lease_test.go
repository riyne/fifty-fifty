package queue_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// effects stands in for what a real handler writes in its transaction. It uses
// audit_events because the app role may insert there; every test gets its own
// kind, so rows can be counted without assuming an empty table.
type effects struct {
	pool *pgxpool.Pool
	kind string
	user uuid.UUID
	grp  uuid.UUID
}

func newEffects(t *testing.T, pool *pgxpool.Pool) *effects {
	t.Helper()
	u, err := db.New(pool).EnsureUser(ctx, db.EnsureUserParams{Issuer: "https://queue-test.example", Subject: testutil.UniqueSubject("fx")})
	if err != nil {
		t.Fatal(err)
	}
	g, err := db.New(pool).CreateGroup(ctx, db.CreateGroupParams{Name: "fx", Currency: "CAD", CreatedBy: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	return &effects{pool: pool, kind: "test.effect." + uuid.NewString(), user: u.ID, grp: g.ID}
}

func (e *effects) write(ctx context.Context, tx pgx.Tx) error {
	return db.New(tx).InsertAuditEvent(ctx, db.InsertAuditEventParams{ActorUserID: e.user, GroupID: e.grp, Kind: e.kind, Data: []byte(`{}`)})
}

func (e *effects) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE kind = $1`, e.kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// staleAndCurrent leaves one job in the situation fencing exists for: worker A
// claimed it, went silent past its lease, and worker B claimed it. Both hold a
// Claimed for the same job with different tokens; only B's is current.
func staleAndCurrent(t *testing.T, pool *pgxpool.Pool, kind string, maxAttempts int) (stale, current queue.Claimed) {
	t.Helper()
	id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: maxAttempts})
	a := claim(t, pool, kind, 1)
	if len(a) != 1 || a[0].ID != id {
		t.Fatalf("worker A did not get the job: %+v", a)
	}
	expireLease(t, pool, id)
	b := claim(t, pool, kind, 1)
	if len(b) != 1 || b[0].ID != id || b[0].LeaseToken == a[0].LeaseToken {
		t.Fatalf("worker B did not take the job over with a new token: %+v", b)
	}
	return a[0], b[0]
}

func completeAlone(t *testing.T, pool *pgxpool.Pool, c queue.Claimed) error {
	t.Helper()
	return queue.CompleteWith(ctx, pool, c, func(context.Context, pgx.Tx) error { return nil })
}

// ---- heartbeat ----

func TestHeartbeat_ExtendsTheLease(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("hb")
	id := enqueue(t, pool, queue.NewJob{Kind: kind})
	c := claim(t, pool, kind, 1)[0]

	exp, err := queue.Heartbeat(ctx, pool, c, 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	row := getJob(t, pool, id)
	if !exp.After(c.LeaseExpiresAt) || !exp.Equal(*row.LeaseExpiresAt) {
		t.Errorf("returned expiry %v, stored %v, previous %v", exp, row.LeaseExpiresAt, c.LeaseExpiresAt)
	}
	if d := *row.UntilLeaseExpires; d < 115*time.Second || d > 120*time.Second {
		t.Errorf("lease now expires in %v, want about 120s", d)
	}
	if !row.RunAt.Equal(*row.LeaseExpiresAt) {
		t.Errorf("run_at %v must move with the lease expiry %v, or the job stays claimable", row.RunAt, row.LeaseExpiresAt)
	}
	// A heartbeating job is not claimable even after its original 30 s would be up.
	if got := claim(t, pool, kind, 1); len(got) != 0 {
		t.Errorf("heartbeating job was claimed: %+v", got)
	}
}

func TestHeartbeat_RejectsBadLease(t *testing.T) {
	pool := testutil.Pool(t)
	if _, err := queue.Heartbeat(ctx, pool, queue.Claimed{ID: uuid.New(), LeaseToken: uuid.New()}, 0); !errors.Is(err, queue.ErrInvalidClaim) {
		t.Errorf("err = %v, want ErrInvalidClaim", err)
	}
}

// Pinned decision: a lease that ran out but was not taken over can still be
// extended by its holder, since the token matches and nobody else has the job.
func TestHeartbeat_RevivesAnExpiredButUnclaimedLease(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("hb-late")
	id := enqueue(t, pool, queue.NewJob{Kind: kind})
	c := claim(t, pool, kind, 1)[0]
	expireLease(t, pool, id)

	if _, err := queue.Heartbeat(ctx, pool, c, time.Minute); err != nil {
		t.Fatalf("late heartbeat: %v", err)
	}
	if got := claim(t, pool, kind, 1); len(got) != 0 {
		t.Errorf("a revived job was claimed: %+v", got)
	}
}

// ---- fencing ----

// A stale worker is the whole reason for the lease token. Whatever it tries, in
// whatever order, the newer attempt is untouched.
func TestFencing_StaleWorkerCannotTouchTheNewAttempt(t *testing.T) {
	pool := testutil.Pool(t)
	stale, current := staleAndCurrent(t, pool, newKind("stale"), 3)
	before := getJob(t, pool, current.ID)

	if _, err := queue.Heartbeat(ctx, pool, stale, time.Hour); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("stale heartbeat: %v, want ErrLeaseLost", err)
	}
	if err := completeAlone(t, pool, stale); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("stale complete: %v, want ErrLeaseLost", err)
	}
	if st, err := queue.Fail(ctx, pool, stale, queue.FailParams{Error: "boom"}); !errors.Is(err, queue.ErrLeaseLost) || st != "" {
		t.Errorf("stale fail: %q, %v, want ErrLeaseLost", st, err)
	}

	after := getJob(t, pool, current.ID)
	if after.State != queue.StateRunning || after.Attempts != 2 || after.LastError != nil ||
		*after.Token != current.LeaseToken || !after.LeaseExpiresAt.Equal(*before.LeaseExpiresAt) || !after.RunAt.Equal(before.RunAt) {
		t.Errorf("the current attempt changed:\n before %+v\n after  %+v", before, after)
	}

	// And it still works for its real holder.
	if _, err := queue.Heartbeat(ctx, pool, current, time.Minute); err != nil {
		t.Errorf("current heartbeat: %v", err)
	}
	if err := completeAlone(t, pool, current); err != nil {
		t.Errorf("current complete: %v", err)
	}
}

// Once a job is finished, even its rightful holder's old token is worthless: a
// late duplicate cannot revive or undo it.
func TestFencing_NothingWorksOnAFinishedJob(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("finished")
	done := enqueue(t, pool, queue.NewJob{Kind: kind})
	dead := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1})

	byID := map[uuid.UUID]queue.Claimed{}
	for _, c := range claim(t, pool, kind, 2) {
		byID[c.ID] = c
	}
	if err := completeAlone(t, pool, byID[done]); err != nil {
		t.Fatal(err)
	}
	if st, err := queue.Fail(ctx, pool, byID[dead], queue.FailParams{Error: "x"}); err != nil || st != queue.StateDead {
		t.Fatalf("setup: %q, %v", st, err)
	}

	for id, want := range map[uuid.UUID]string{done: queue.StateSucceeded, dead: queue.StateDead} {
		c := byID[id]
		if _, err := queue.Heartbeat(ctx, pool, c, time.Minute); !errors.Is(err, queue.ErrLeaseLost) {
			t.Errorf("%s heartbeat: %v", want, err)
		}
		if err := completeAlone(t, pool, c); !errors.Is(err, queue.ErrLeaseLost) {
			t.Errorf("%s complete: %v", want, err)
		}
		if _, err := queue.Fail(ctx, pool, c, queue.FailParams{Error: "again"}); !errors.Is(err, queue.ErrLeaseLost) {
			t.Errorf("%s fail: %v", want, err)
		}
		if row := getJob(t, pool, id); row.State != want {
			t.Errorf("state %q, want it to stay %q", row.State, want)
		}
	}
}

func TestFencing_WrongTokenOrWrongJob(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("wrong")
	a := enqueue(t, pool, queue.NewJob{Kind: kind})
	enqueue(t, pool, queue.NewJob{Kind: kind})
	got := claim(t, pool, kind, 2)
	if len(got) != 2 {
		t.Fatal("setup")
	}
	var mine, other queue.Claimed
	if got[0].ID == a {
		mine, other = got[0], got[1]
	} else {
		mine, other = got[1], got[0]
	}
	forged := mine
	forged.LeaseToken = uuid.New()
	crossed := mine
	crossed.LeaseToken = other.LeaseToken // a valid token, but for a different job

	for name, c := range map[string]queue.Claimed{"forged token": forged, "another job's token": crossed} {
		if _, err := queue.Heartbeat(ctx, pool, c, time.Minute); !errors.Is(err, queue.ErrLeaseLost) {
			t.Errorf("%s heartbeat: %v", name, err)
		}
		if err := completeAlone(t, pool, c); !errors.Is(err, queue.ErrLeaseLost) {
			t.Errorf("%s complete: %v", name, err)
		}
		if _, err := queue.Fail(ctx, pool, c, queue.FailParams{}); !errors.Is(err, queue.ErrLeaseLost) {
			t.Errorf("%s fail: %v", name, err)
		}
	}
	if row := getJob(t, pool, mine.ID); row.State != queue.StateRunning || *row.Token != mine.LeaseToken {
		t.Errorf("job changed: %+v", row)
	}
}

// ---- effects and completion are one transaction ----

func TestCompleteWith_EffectsAndCompletionCommitTogether(t *testing.T) {
	pool := testutil.Pool(t)
	fx := newEffects(t, pool)
	kind := newKind("atomic")
	id := enqueue(t, pool, queue.NewJob{Kind: kind})
	c := claim(t, pool, kind, 1)[0]

	if err := queue.CompleteWith(ctx, pool, c, fx.write); err != nil {
		t.Fatal(err)
	}
	if fx.count(t) != 1 {
		t.Errorf("%d effects, want 1", fx.count(t))
	}
	if row := getJob(t, pool, id); row.State != queue.StateSucceeded || row.FinishedAt == nil || row.Token != nil || row.LeaseExpiresAt != nil {
		t.Errorf("row = %+v, want succeeded, finished, no lease", row)
	}
}

var errHandler = errors.New("handler failed")

func TestCompleteWith_HandlerErrorDiscardsEffectsAndLeavesTheJob(t *testing.T) {
	pool := testutil.Pool(t)
	fx := newEffects(t, pool)
	kind := newKind("atomic-fail")
	id := enqueue(t, pool, queue.NewJob{Kind: kind})
	c := claim(t, pool, kind, 1)[0]

	err := queue.CompleteWith(ctx, pool, c, func(ctx context.Context, tx pgx.Tx) error {
		if err := fx.write(ctx, tx); err != nil {
			return err
		}
		return errHandler // fails after writing
	})
	if !errors.Is(err, errHandler) {
		t.Fatalf("err = %v, want the handler's error", err)
	}
	if fx.count(t) != 0 {
		t.Errorf("a failed handler's effects survived: %d rows", fx.count(t))
	}
	if row := getJob(t, pool, id); row.State != queue.StateRunning || *row.Token != c.LeaseToken {
		t.Errorf("job changed by a failed handler: %+v", row)
	}
	// The caller then reports the failure.
	if st, err := queue.Fail(ctx, pool, c, queue.FailParams{Error: err.Error()}); err != nil || st != queue.StateQueued {
		t.Errorf("fail after rollback: %q, %v", st, err)
	}
}

// The scenario fencing exists for. Worker A starts, writes its effect inside its
// transaction, then stalls (GC pause, partition, SIGSTOP) past its lease. B takes
// the job over and finishes it. A wakes up and tries to complete. A's effect must
// not exist: exactly one worker's effects are committed.
func TestCompleteWith_PausedWorkersEffectsAreDiscarded(t *testing.T) {
	pool := testutil.Pool(t)
	fx := newEffects(t, pool)
	kind := newKind("paused")
	id := enqueue(t, pool, queue.NewJob{Kind: kind})
	a := claim(t, pool, kind, 1)[0]

	aWrote, resume := make(chan struct{}), make(chan struct{})
	aResult := make(chan error, 1)
	go func() {
		aResult <- queue.CompleteWith(ctx, pool, a, func(ctx context.Context, tx pgx.Tx) error {
			if err := fx.write(ctx, tx); err != nil {
				return err
			}
			close(aWrote)
			<-resume // stalled
			return nil
		})
	}()
	<-aWrote

	expireLease(t, pool, id)
	b := claim(t, pool, kind, 1)
	if len(b) != 1 {
		t.Fatal("B could not take the job over")
	}
	if err := queue.CompleteWith(ctx, pool, b[0], fx.write); err != nil {
		t.Fatalf("B: %v", err)
	}

	close(resume)
	if err := <-aResult; !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("A: %v, want ErrLeaseLost", err)
	}
	if n := fx.count(t); n != 1 {
		t.Errorf("%d committed effects, want exactly 1 (B's)", n)
	}
	if row := getJob(t, pool, id); row.State != queue.StateSucceeded || row.Attempts != 2 {
		t.Errorf("row = %+v", row)
	}
}

// A worker that is late but was not replaced may still finish, and once its
// completion statement has run nobody can take the job from under it before it
// commits: the completion holds the row, and claims skip held rows.
func TestComplete_HoldsTheRowSoNobodyTakesOverMidCommit(t *testing.T) {
	pool := testutil.Pool(t)
	fx := newEffects(t, pool)
	kind := newKind("midcommit")
	id := enqueue(t, pool, queue.NewJob{Kind: kind})
	c := claim(t, pool, kind, 1)[0]
	expireLease(t, pool, id) // overdue, but nobody has claimed it yet

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fx.write(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(ctx, tx, c); err != nil {
		t.Fatal(err)
	}

	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	got, err := queue.ClaimJobs(bounded, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: 1, Lease: time.Minute})
	if err != nil || len(got) != 0 {
		t.Fatalf("a job being completed was claimed (or the claim waited): %+v, %v", got, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if row := getJob(t, pool, id); row.State != queue.StateSucceeded || fx.count(t) != 1 {
		t.Errorf("row = %+v, effects = %d", row, fx.count(t))
	}
	if got := claim(t, pool, kind, 1); len(got) != 0 {
		t.Errorf("a finished job was claimed: %+v", got)
	}
}

// Both writes run on one row; the row lock makes exactly one of them take effect.
func TestFencing_CompleteAndFailRaceHasOneWinner(t *testing.T) {
	pool := testutil.Pool(t)
	kind := newKind("racewin")
	for round := range 40 {
		id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 3})
		c := claim(t, pool, kind, 1)[0]

		start := make(chan struct{})
		var wg sync.WaitGroup
		var completeErr, failErr error
		wg.Go(func() { <-start; completeErr = completeAlone(t, pool, c) })
		wg.Go(func() { <-start; _, failErr = queue.Fail(ctx, pool, c, queue.FailParams{Error: "x"}) })
		close(start)
		wg.Wait()

		completed, failed := completeErr == nil, failErr == nil
		if completed == failed {
			t.Fatalf("round %d: complete err=%v, fail err=%v; exactly one must win", round, completeErr, failErr)
		}
		loser := completeErr
		want := queue.StateQueued
		if completed {
			loser, want = failErr, queue.StateSucceeded
		}
		if !errors.Is(loser, queue.ErrLeaseLost) {
			t.Fatalf("round %d: loser err = %v, want ErrLeaseLost", round, loser)
		}
		if row := getJob(t, pool, id); row.State != want {
			t.Fatalf("round %d: state %q, want %q", round, row.State, want)
		}
	}
}

// ---- fail ----

func TestFail_RequeuesOrDeadLetters(t *testing.T) {
	pool := testutil.Pool(t)

	t.Run("an attempt left: queued for later, not claimable until then", func(t *testing.T) {
		kind := newKind("fail-retry")
		id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 2})
		c := claim(t, pool, kind, 1)[0]

		st, err := queue.Fail(ctx, pool, c, queue.FailParams{Error: "temporary", RetryIn: time.Hour})
		if err != nil || st != queue.StateQueued {
			t.Fatalf("%q, %v", st, err)
		}
		row := getJob(t, pool, id)
		var in time.Duration // run_at - now(), on the database clock
		if err := pool.QueryRow(ctx, `SELECT run_at - now() FROM jobs WHERE id = $1`, id).Scan(&in); err != nil {
			t.Fatal(err)
		}
		if in < 59*time.Minute || in > time.Hour {
			t.Errorf("retry scheduled %v from now, want about 1h", in)
		}
		if row.State != queue.StateQueued || row.Attempts != 1 ||
			row.Token != nil || row.LeaseExpiresAt != nil || row.FinishedAt != nil || row.LastError == nil || *row.LastError != "temporary" {
			t.Errorf("row = %+v", row)
		}
		if got := claim(t, pool, kind, 1); len(got) != 0 {
			t.Fatalf("claimed before its retry time: %+v", got)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET run_at = now() WHERE id = $1`, id); err != nil { // time passes
			t.Fatal(err)
		}
		got := claim(t, pool, kind, 1)
		if len(got) != 1 || got[0].Attempt != 2 || got[0].LeaseToken == c.LeaseToken {
			t.Errorf("retry claim = %+v, want attempt 2 with a new token", got)
		}
	})

	t.Run("a negative delay is refused before touching the job", func(t *testing.T) {
		kind := newKind("fail-neg")
		id := enqueue(t, pool, queue.NewJob{Kind: kind})
		c := claim(t, pool, kind, 1)[0]
		if _, err := queue.Fail(ctx, pool, c, queue.FailParams{RetryIn: -time.Second}); !errors.Is(err, queue.ErrInvalidClaim) {
			t.Errorf("err = %v, want ErrInvalidClaim", err)
		}
		if row := getJob(t, pool, id); row.State != queue.StateRunning {
			t.Errorf("job changed: %+v", row)
		}
	})

	t.Run("no retry delay means due now", func(t *testing.T) {
		kind := newKind("fail-now")
		enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 2})
		c := claim(t, pool, kind, 1)[0]
		if _, err := queue.Fail(ctx, pool, c, queue.FailParams{Error: "x"}); err != nil {
			t.Fatal(err)
		}
		if got := claim(t, pool, kind, 1); len(got) != 1 || got[0].Attempt != 2 {
			t.Errorf("claim = %+v", got)
		}
	})

	t.Run("the last attempt: dead, with the reason", func(t *testing.T) {
		kind := newKind("fail-dead")
		id := enqueue(t, pool, queue.NewJob{Kind: kind, MaxAttempts: 1})
		c := claim(t, pool, kind, 1)[0]

		st, err := queue.Fail(ctx, pool, c, queue.FailParams{Error: "gave up", RetryIn: time.Second})
		if err != nil || st != queue.StateDead {
			t.Fatalf("%q, %v", st, err)
		}
		row := getJob(t, pool, id)
		if row.State != queue.StateDead || row.Attempts != 1 || row.FinishedAt == nil || row.Token != nil || row.LastError == nil || *row.LastError != "gave up" {
			t.Errorf("row = %+v", row)
		}
		if got := claim(t, pool, kind, 1); len(got) != 0 {
			t.Errorf("a dead job was claimed: %+v", got)
		}
	})

	t.Run("a long error is cut on a character boundary", func(t *testing.T) {
		kind := newKind("fail-long")
		id := enqueue(t, pool, queue.NewJob{Kind: kind})
		c := claim(t, pool, kind, 1)[0]
		// 3-byte characters, so the byte limit falls inside one.
		long := strings.Repeat("€", queue.MaxErrorLen)
		if _, err := queue.Fail(ctx, pool, c, queue.FailParams{Error: long}); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := pool.QueryRow(ctx, `SELECT last_error FROM jobs WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if len(got) > queue.MaxErrorLen || len(got) < queue.MaxErrorLen-3 || strings.ContainsRune(got, '�') || got != strings.Repeat("€", len(got)/3) {
			t.Errorf("stored error is %d bytes: not a clean cut of the input", len(got))
		}
	})
}
