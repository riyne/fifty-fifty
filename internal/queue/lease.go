package queue

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
)

// ErrLeaseLost means the caller no longer holds the job: its lease was taken
// over by another claim, or the job already finished or was dead-lettered. The
// caller must stop working on it and discard anything it has not committed.
// Nothing about the job was changed.
var ErrLeaseLost = errors.New("lease lost")

// MaxErrorLen bounds the stored last_error, in bytes.
const MaxErrorLen = 2000

// Heartbeat extends the lease on a claimed job by lease from now (database
// clock) and returns the new expiry. Call it well within the lease, for example
// every lease/3. ErrLeaseLost means the job is no longer yours.
func Heartbeat(ctx context.Context, pool *pgxpool.Pool, c Claimed, lease time.Duration) (time.Time, error) {
	if lease <= 0 {
		return time.Time{}, fmt.Errorf("%w: lease must be positive", ErrInvalidClaim)
	}
	exp, err := db.New(pool).HeartbeatJob(ctx, db.HeartbeatJobParams{ID: c.ID, LeaseToken: c.LeaseToken, LeaseSeconds: lease.Seconds()})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrLeaseLost
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("heartbeat job: %w", err)
	}
	if exp == nil {
		return time.Time{}, errors.New("heartbeat job: no expiry returned") // unreachable: column is set while running
	}
	return *exp, nil
}

// Complete marks the claimed job succeeded, inside tx. It fails with
// ErrLeaseLost, and the caller must then roll tx back, if the lease is no longer
// held.
//
// Call it LAST in the transaction that makes the job's effects, or just use
// CompleteWith, which does. Two reasons it has to be last and in the same
// transaction as the effects:
//
//   - Atomic: the effects and "this job is done" commit together or not at all,
//     so a crash can leave neither (the job is retried) or both, never only the
//     effects (which a retry would then repeat).
//   - Fenced: this statement is the check that the lease is still ours, and its
//     row lock stops anyone claiming the job until we commit. If another worker
//     took the job over while we were busy, this finds no matching row and the
//     caller's rollback discards our effects. Checking the lease first and then
//     doing the effects would leave a gap in which the lease could be lost.
func Complete(ctx context.Context, tx pgx.Tx, c Claimed) error {
	n, err := db.New(tx).CompleteJob(ctx, db.CompleteJobParams{ID: c.ID, LeaseToken: c.LeaseToken})
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if n != 1 {
		return ErrLeaseLost
	}
	return nil
}

// CompleteWith runs fn and completes the job in one transaction.
//
//   - fn returns nil and the lease is still held: fn's writes and the completion
//     commit together.
//   - fn returns an error: everything is rolled back, the job is untouched
//     (still running under our lease), and fn's error is returned as is. The
//     caller should then call Fail.
//   - the lease was lost: everything is rolled back and ErrLeaseLost is returned.
//     fn's effects never happened, which is what makes a stale worker harmless.
//
// fn must not commit or roll back tx. Anything fn does outside tx (an HTTP call,
// say) is not covered: that part is at-least-once.
func CompleteWith(ctx context.Context, pool *pgxpool.Pool, c Claimed, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := Complete(ctx, tx, c); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// FailParams says what happens to a failed attempt.
type FailParams struct {
	// Error is stored as last_error, cut to MaxErrorLen bytes. Do not put
	// secrets or receipt text in it.
	Error string
	// RetryAt is when the job may run again if it has an attempt left. Zero
	// means now. The backoff policy that picks it is M3.4.
	RetryAt time.Time
}

// Fail ends the current attempt of a claimed job: it goes back to queued, to
// run again at p.RetryAt, or to dead when this was its last attempt. Returns the
// state the job ended in (StateQueued or StateDead). ErrLeaseLost means the job
// is no longer yours and was not changed, so a stale worker's failure cannot
// undo a newer attempt's success.
//
// It takes the pool because it belongs after the attempt's transaction was
// rolled back, never inside it.
func Fail(ctx context.Context, pool *pgxpool.Pool, c Claimed, p FailParams) (string, error) {
	arg := db.FailJobParams{ID: c.ID, LeaseToken: c.LeaseToken, LastError: truncate(p.Error, MaxErrorLen)}
	if !p.RetryAt.IsZero() {
		arg.RetryAt = &p.RetryAt
	}
	state, err := db.New(pool).FailJob(ctx, arg)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrLeaseLost
	}
	if err != nil {
		return "", fmt.Errorf("fail job: %w", err)
	}
	return state, nil
}

// truncate cuts s to at most n bytes without splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
