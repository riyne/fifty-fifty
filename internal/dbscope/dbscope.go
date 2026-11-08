// Package dbscope is how request handlers reach the database on behalf of a
// user. Every statement a handler runs goes through a transaction in which the
// setting app.user_id holds the caller's id, so that row-level security (M7.2)
// can tell whose request it is.
//
// The setting is transaction-local (set_config with is_local = true), so it
// disappears at COMMIT or ROLLBACK and a pooled connection never carries one
// user's identity into the next request. A handler holds a *DB, which has no
// way to hand out the pool, so it cannot run a statement outside this seam.
//
// The identity is self-asserted: code running as the application role can set
// app.user_id to anything. Row-level security protects against a query that
// forgot its membership check, not against code that sets the wrong identity.
package dbscope

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
)

// Setting is the name of the transaction-local setting that carries the caller.
const Setting = "app.user_id"

// DefaultLockTimeout is how long a statement of a user request may wait for a lock that
// another transaction holds (a row lock, or the idempotency key another request is
// still inserting) before Postgres cancels it with SQLSTATE 55P03. A request normally
// holds its locks for milliseconds, so a wait of seconds means the holder has stalled;
// without a limit the waiting requests would pile up behind it for as long as it lives.
// The transaction rolls back and nothing is stored under the idempotency key, so the
// client's retry with the same key is a fresh attempt. The api answers 503 `busy`
// (httpx.InternalError). A placeholder, not tuned.
const DefaultLockTimeout = 5 * time.Second

// shortened is a test's shorter timeout in nanoseconds; 0 means DefaultLockTimeout.
var shortened atomic.Int64

// ShortenLockTimeout makes the lock timeout d until the test ends, so that a test can
// hold a raw lock for a fraction of a second and see a request give up. It can only
// shorten the wait: a d of zero or less, or one not below the default, panics, so that
// nothing can use it to switch the limit off. A test that calls it must not run in
// parallel with another that depends on the default.
func ShortenLockTimeout(t interface{ Cleanup(func()) }, d time.Duration) {
	if d <= 0 || d >= DefaultLockTimeout {
		panic("dbscope.ShortenLockTimeout: the timeout may only be made shorter than the default, and above zero")
	}
	shortened.Store(int64(d))
	t.Cleanup(func() { shortened.Store(0) })
}

func lockTimeout() time.Duration {
	if s := shortened.Load(); s > 0 {
		return time.Duration(s)
	}
	return DefaultLockTimeout
}

// DB runs statements on behalf of one user at a time.
type DB struct {
	pool *pgxpool.Pool
}

// New wraps pool. The pool is not exposed again.
func New(pool *pgxpool.Pool) *DB { return &DB{pool: pool} }

// Begin starts a transaction (default options) in which app.user_id is userID.
func (d *DB) Begin(ctx context.Context, userID uuid.UUID) (pgx.Tx, error) {
	return d.BeginTx(ctx, userID, pgx.TxOptions{})
}

// BeginTx is Begin with transaction options.
func (d *DB) BeginTx(ctx context.Context, userID uuid.UUID, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	// The third argument is is_local: the value lasts until the end of this
	// transaction and no longer. Passing false would make it last for the whole
	// connection, which is the leak this package exists to prevent. The lock timeout
	// is set the same way, in the same statement (no extra round trip), for the same
	// reason: it must not outlive the transaction on a pooled connection.
	if _, err := tx.Exec(ctx, "SELECT set_config('"+Setting+"', $1, true), set_config('lock_timeout', $2, true)",
		userID.String(), strconv.FormatInt(lockTimeout().Milliseconds(), 10)); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("set %s and the lock timeout: %w", Setting, err)
	}
	return tx, nil
}

// Reader returns Queries for single-statement reads made as userID. Each
// statement runs in its own short read-only transaction with the identity set,
// which is what an autocommit statement was before, plus the identity. A
// statement that needs a lock, several statements that must agree, or a write
// uses Begin instead; the database refuses a write made through a Reader.
func (d *DB) Reader(userID uuid.UUID) *db.Queries {
	return db.New(&reader{d: d, userID: userID})
}

// reader implements db.DBTX with one read-only transaction per statement.
type reader struct {
	d      *DB
	userID uuid.UUID
}

func (r *reader) begin(ctx context.Context) (pgx.Tx, error) {
	return r.d.BeginTx(ctx, r.userID, pgx.TxOptions{AccessMode: pgx.ReadOnly})
}

func (r *reader) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		_ = tx.Rollback(ctx)
		return tag, err
	}
	return tag, tx.Commit(ctx)
}

func (r *reader) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return &txRows{Rows: rows, tx: tx, ctx: ctx}, nil
}

func (r *reader) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := r.Query(ctx, sql, args...)
	return &txRow{rows: rows, err: err}
}

// txRows ends its transaction when the rows are closed. sqlc-generated code
// always closes its rows (defer rows.Close()), and a commit that fails is
// reported through Err, which it always checks.
type txRows struct {
	pgx.Rows
	tx        pgx.Tx
	ctx       context.Context
	closed    bool
	commitErr error
}

func (r *txRows) Close() {
	if r.closed {
		return
	}
	r.closed = true
	r.Rows.Close()
	if r.Rows.Err() != nil {
		_ = r.tx.Rollback(r.ctx)
		return
	}
	r.commitErr = r.tx.Commit(r.ctx)
}

func (r *txRows) Err() error {
	if err := r.Rows.Err(); err != nil {
		return err
	}
	return r.commitErr
}

// txRow is pgx's single-row Scan on top of txRows.
type txRow struct {
	rows pgx.Rows
	err  error
}

func (r *txRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	if err := r.rows.Scan(dest...); err != nil {
		return err
	}
	r.rows.Close()
	return r.rows.Err()
}
