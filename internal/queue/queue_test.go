package queue_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/dbscope"
	"github.com/riyne/fifty-fifty/internal/idempotency"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

var ctx = context.Background()

// Tests share one database that is never cleaned, so every test tags its jobs
// with a payload marker nobody else uses and counts only those.
func marker() string { return uuid.NewString() }

func jobsWithMarker(t *testing.T, pool *pgxpool.Pool, m string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE payload->>'marker' = $1`, m).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func inTx(t *testing.T, pool *pgxpool.Pool, commit bool, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	fn(tx)
	if commit {
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnqueue_CommitStoresAQueuedJob(t *testing.T) {
	pool := testutil.WorkerPool(t)
	m := marker()
	var id uuid.UUID
	var err error
	inTx(t, pool, true, func(tx pgx.Tx) {
		id, err = queue.Enqueue(ctx, tx, queue.NewJob{Kind: "test.commit", Payload: map[string]any{"marker": m, "n": 7}})
		if err != nil {
			t.Fatal(err)
		}
	})

	var (
		kind, state, payloadMarker string
		attempts, maxAttempts      int
		leaseToken                 *uuid.UUID
		leaseExpires, finished     *time.Time
		lastError                  *string
		skew                       time.Duration
	)
	err = pool.QueryRow(ctx, `
		SELECT kind, state, payload->>'marker', attempts, max_attempts, lease_token, lease_expires_at, finished_at, last_error,
		       now() - run_at
		FROM jobs WHERE id = $1`, id).
		Scan(&kind, &state, &payloadMarker, &attempts, &maxAttempts, &leaseToken, &leaseExpires, &finished, &lastError, &skew)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "test.commit" || state != queue.StateQueued || payloadMarker != m {
		t.Errorf("row = %q %q %q", kind, state, payloadMarker)
	}
	if attempts != 0 || maxAttempts != queue.DefaultMaxAttempts {
		t.Errorf("attempts=%d max_attempts=%d, want 0 and %d", attempts, maxAttempts, queue.DefaultMaxAttempts)
	}
	if leaseToken != nil || leaseExpires != nil || finished != nil || lastError != nil {
		t.Errorf("a new job must carry no lease, finish time or error")
	}
	// run_at defaults to the database clock, so it is already due when the
	// transaction has committed.
	if skew < 0 || skew > time.Minute {
		t.Errorf("run_at is %v before the database's now(), want due now", skew)
	}
}

// The reason Enqueue takes a transaction: the job vanishes with it.
func TestEnqueue_RollbackLeavesNoJob(t *testing.T) {
	pool := testutil.WorkerPool(t)
	m := marker()
	inTx(t, pool, false, func(tx pgx.Tx) {
		if _, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: "test.rollback", Payload: map[string]any{"marker": m}}); err != nil {
			t.Fatal(err)
		}
		// Not visible to anyone else before commit.
		if n := jobsWithMarker(t, pool, m); n != 0 {
			t.Errorf("uncommitted job visible to another connection: %d", n)
		}
	}) // rolled back by the deferred Rollback
	if n := jobsWithMarker(t, pool, m); n != 0 {
		t.Fatalf("rolled-back job survived: %d rows", n)
	}
}

func TestEnqueue_RunAtAndMaxAttempts(t *testing.T) {
	pool := testutil.WorkerPool(t)
	m := marker()
	at := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	var id uuid.UUID
	inTx(t, pool, true, func(tx pgx.Tx) {
		var err error
		id, err = queue.Enqueue(ctx, tx, queue.NewJob{Kind: "test.opts", Payload: map[string]any{"marker": m}, RunAt: at, MaxAttempts: 2})
		if err != nil {
			t.Fatal(err)
		}
	})
	var runAt time.Time
	var maxAttempts int
	if err := pool.QueryRow(ctx, `SELECT run_at, max_attempts FROM jobs WHERE id = $1`, id).Scan(&runAt, &maxAttempts); err != nil {
		t.Fatal(err)
	}
	if !runAt.Equal(at) || maxAttempts != 2 {
		t.Errorf("run_at=%v max_attempts=%d, want %v and 2", runAt, maxAttempts, at)
	}
}

// A nil payload is an empty object, not a JSON null (which the table refuses).
func TestEnqueue_NilPayloadIsEmptyObject(t *testing.T) {
	pool := testutil.WorkerPool(t)
	var id uuid.UUID
	inTx(t, pool, true, func(tx pgx.Tx) {
		var err error
		if id, err = queue.Enqueue(ctx, tx, queue.NewJob{Kind: "test.nilpayload"}); err != nil {
			t.Fatal(err)
		}
	})
	var p string
	if err := pool.QueryRow(ctx, `SELECT payload::text FROM jobs WHERE id = $1`, id).Scan(&p); err != nil || p != "{}" {
		t.Fatalf("payload = %q, %v", p, err)
	}
}

func TestEnqueue_RejectsInvalidJobs(t *testing.T) {
	pool := testutil.WorkerPool(t)
	long := make([]byte, queue.MaxKindLen+1)
	for i := range long {
		long[i] = 'k'
	}
	cases := []struct {
		name string
		job  func(m string) queue.NewJob
	}{
		{"empty kind", func(m string) queue.NewJob { return queue.NewJob{Payload: map[string]any{"marker": m}} }},
		{"kind too long", func(m string) queue.NewJob {
			return queue.NewJob{Kind: string(long), Payload: map[string]any{"marker": m}}
		}},
		{"negative max attempts", func(m string) queue.NewJob {
			return queue.NewJob{Kind: "k", MaxAttempts: -1, Payload: map[string]any{"marker": m}}
		}},
		{"max attempts beyond int32", func(m string) queue.NewJob {
			return queue.NewJob{Kind: "k", MaxAttempts: 1 << 40, Payload: map[string]any{"marker": m}}
		}},
		{"array payload", func(m string) queue.NewJob { return queue.NewJob{Kind: "k", Payload: []string{m}} }},
		{"string payload", func(m string) queue.NewJob { return queue.NewJob{Kind: "k", Payload: m} }},
		{"unmarshalable payload", func(m string) queue.NewJob { return queue.NewJob{Kind: "k", Payload: make(chan int)} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := marker()
			inTx(t, pool, true, func(tx pgx.Tx) {
				_, err := queue.Enqueue(ctx, tx, c.job(m))
				if !errors.Is(err, queue.ErrInvalidJob) {
					t.Fatalf("err = %v, want ErrInvalidJob", err)
				}
			})
			if n := jobsWithMarker(t, pool, m); n != 0 {
				t.Fatalf("%d rows written for an invalid job", n)
			}
		})
	}
}

// The pattern M4 (settlements) and M6 (receipts) will use: the enqueue is part
// of an idempotent request, so a retried or duplicated request does not enqueue
// a second job. Ten concurrent requests with one key must leave exactly one job.
func TestEnqueue_InsideIdempotentRequestEnqueuesOnce(t *testing.T) {
	// The request is a user's, so it runs on the application role's pool; the
	// count of jobs it left is read as the worker, which owns the queue.
	app, pool := testutil.Pool(t), testutil.WorkerPool(t)
	user, err := db.New(testutil.SharedOwnerPool(t)).EnsureUser(ctx, db.EnsureUserParams{
		Issuer: "https://queue-test.example", Subject: testutil.UniqueSubject("q"),
	})
	if err != nil {
		t.Fatal(err)
	}
	m, key := marker(), uuid.NewString()
	fp := idempotency.Fingerprint("POST", "/test", m)

	const n = 10
	var wg sync.WaitGroup
	replayed := make(chan bool, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := idempotency.Do(ctx, dbscope.New(app), user.ID, key, fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
				if _, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: "test.idem", Payload: map[string]any{"marker": m}}); err != nil {
					return idempotency.Result{}, err
				}
				return idempotency.Result{Status: http.StatusAccepted, Body: []byte(`{}`)}, nil
			})
			errs <- err
			replayed <- out.Replayed
		}()
	}
	wg.Wait()
	close(errs)
	close(replayed)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := 0
	for r := range replayed {
		if !r {
			first++
		}
	}
	if first != 1 {
		t.Errorf("%d requests ran the handler, want 1", first)
	}
	if got := jobsWithMarker(t, pool, m); got != 1 {
		t.Fatalf("%d jobs enqueued by %d duplicate requests, want 1", got, n)
	}
}
