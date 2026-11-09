package dbscope

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/testutil"
)

// onePool is a pool with a single connection, so every statement reuses it and
// a setting that survived a transaction would be seen by the next one.
func onePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testutil.Pool(t) // skips, or fails under REQUIRE_DATABASE=1, when there is no database
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Close waits for every connection to come back; a test that found a leak
	// must still be able to finish and say so.
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { pool.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	return pool
}

// tctx bounds a test: a statement that waits for the pool's only connection
// (because an earlier one never released it) fails after 5 seconds.
func tctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// current reads the setting. After a transaction that set it ended, Postgres
// reports an empty string (or NULL), never the old value.
func current(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) string {
	t.Helper()
	var v *string
	if err := q.QueryRow(tctx(t), "SELECT current_setting('"+Setting+"', true)").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func TestBegin_SetsTheIdentityAndEndsItWithTheTransaction(t *testing.T) {
	pool := onePool(t)
	d := New(pool)
	ctx := tctx(t)
	a, b := uuid.New(), uuid.New()

	for _, end := range []string{"commit", "rollback"} {
		tx, err := d.Begin(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if got := current(t, tx); got != a.String() {
			t.Fatalf("inside the transaction: got %q, want %q", got, a)
		}
		if end == "commit" {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		// The same single connection, outside any scope.
		if got := current(t, pool); got != "" {
			t.Fatalf("after %s the connection still carries %q", end, got)
		}
	}

	// The next user on the same connection sees only their own identity.
	tx, err := d.Begin(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if got := current(t, tx); got != b.String() {
		t.Fatalf("got %q, want %q", got, b)
	}
}

func TestBegin_AFailedTransactionDoesNotLeakEither(t *testing.T) {
	pool := onePool(t)
	d := New(pool)
	ctx := tctx(t)
	tx, err := d.Begin(ctx, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT no_such_function()"); err == nil {
		t.Fatal("expected an error")
	}
	_ = tx.Rollback(ctx)
	if got := current(t, pool); got != "" {
		t.Fatalf("connection still carries %q", got)
	}
}

func TestBegin_ACancelledContextReturnsTheConnection(t *testing.T) {
	pool := onePool(t)
	d := New(pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Begin(ctx, uuid.New()); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d connections still held", n)
	}
	// And the pool still works.
	if got := current(t, pool); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestReader_RunsAsTheUserAndReleasesTheConnection(t *testing.T) {
	pool := onePool(t)
	r := &reader{d: New(pool), userID: uuid.New()}
	ctx := tctx(t)

	if got := current(t, r); got != r.userID.String() {
		t.Fatalf("QueryRow: got %q, want %q", got, r.userID)
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("QueryRow left %d connections held", n)
	}

	rows, err := r.Query(ctx, "SELECT g, current_setting('"+Setting+"', true) FROM generate_series(1, 3) g")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var g int
		var who string
		if err := rows.Scan(&g, &who); err != nil {
			t.Fatal(err)
		}
		if who != r.userID.String() {
			t.Fatalf("row %d ran as %q", g, who)
		}
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil || n != 3 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	rows.Close() // closing twice is harmless
	if got := pool.Stat().AcquiredConns(); got != 0 {
		t.Fatalf("Query left %d connections held", got)
	}

	if got := current(t, pool); got != "" {
		t.Fatalf("connection still carries %q", got)
	}
}

func TestReader_NoRowsAndErrorsReleaseTheConnection(t *testing.T) {
	pool := onePool(t)
	r := &reader{d: New(pool), userID: uuid.New()}
	ctx := tctx(t)

	var x int
	if err := r.QueryRow(ctx, "SELECT 1 WHERE false").Scan(&x); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("got %v, want ErrNoRows", err)
	}
	if err := r.QueryRow(ctx, "SELECT no_such_function()").Scan(&x); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := r.Query(ctx, "SELECT no_such_function()"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := r.Exec(ctx, "SELECT no_such_function()"); err == nil {
		t.Fatal("expected an error")
	}
	// A scan error must still end the transaction.
	var bad bool
	if err := r.QueryRow(ctx, "SELECT 'x'::text").Scan(&bad); err == nil {
		t.Fatal("expected a scan error")
	}

	if got := pool.Stat().AcquiredConns(); got != 0 {
		t.Fatalf("%d connections still held", got)
	}
	if got := current(t, pool); got != "" {
		t.Fatalf("connection still carries %q", got)
	}
}

func TestReader_RefusesWrites(t *testing.T) {
	pool := onePool(t)
	r := &reader{d: New(pool), userID: uuid.New()}
	_, err := r.Exec(tctx(t), "INSERT INTO groups (name, currency, created_by) VALUES ('x', 'CAD', $1)", uuid.New())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" { // read_only_sql_transaction
		t.Fatalf("got %v, want SQLSTATE 25006", err)
	}
	if got := pool.Stat().AcquiredConns(); got != 0 {
		t.Fatalf("%d connections still held", got)
	}
}

// ---- lock timeout ----

// A request transaction gives up on a lock another transaction holds. An advisory lock
// held by a raw owner transaction stands for any lock (a row lock, the unique index of
// an idempotency key); Postgres treats them alike for lock_timeout.
func TestBegin_LockWaitEndsAtTheLockTimeout(t *testing.T) {
	ShortenLockTimeout(t, 300*time.Millisecond)
	owner := testutil.SharedOwnerPool(t)
	ctx := tctx(t)
	const lockID = 7_100_014

	holder, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // cleanup
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID); err != nil {
		t.Fatal(err)
	}

	tx, err := New(onePool(t)).Begin(ctx, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // cleanup
	start := time.Now()
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID)
	waited := time.Since(start)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("waiting for a held lock: err = %v after %v, want SQLSTATE 55P03 (lock timeout)", err, waited)
	}
	if waited < 250*time.Millisecond || waited > 3*time.Second {
		t.Errorf("gave up after %v, want about the 300 ms timeout", waited)
	}
}

// The timeout is transaction-local like the identity: a pooled connection must not carry
// it into the next use (the same single connection is reused here).
func TestBegin_LockTimeoutEndsWithTheTransaction(t *testing.T) {
	pool := onePool(t)
	ctx := tctx(t)
	var before string
	if err := pool.QueryRow(ctx, `SHOW lock_timeout`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := New(pool).Begin(ctx, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	var inside string
	if err := tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&inside); err != nil {
		t.Fatal(err)
	}
	if inside != "5s" {
		t.Errorf("inside the transaction lock_timeout = %q, want 5s", inside)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := pool.QueryRow(ctx, `SHOW lock_timeout`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("after the transaction lock_timeout = %q, want it back to %q: the setting leaked", after, before)
	}
}

// Nothing may use the test hook to switch the protection off.
func TestShortenLockTimeout_OnlyShortens(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, DefaultLockTimeout, 2 * DefaultLockTimeout} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("ShortenLockTimeout(%v) did not panic", d)
				}
			}()
			ShortenLockTimeout(t, d)
		}()
	}
	if lockTimeout() != DefaultLockTimeout {
		t.Errorf("a refused call changed the timeout to %v", lockTimeout())
	}
}
