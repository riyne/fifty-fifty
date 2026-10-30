// Package queue is the Postgres-backed job queue (DESIGN.md section 6).
//
// Jobs are rows in the jobs table. This file is the producer side: Enqueue. It
// takes a pgx.Tx, not a pool, on purpose. The job must commit or roll back with
// the row that needs it (a settlement, a receipt upload), so there is no window
// where the row exists and its job was lost, or the job exists for a row that
// was rolled back. Passing a transaction makes the compiler enforce that; a
// caller cannot enqueue "on the side".
package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
)

// Job states, as stored in jobs.state.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateDead      = "dead"
)

const (
	// DefaultMaxAttempts is used when NewJob.MaxAttempts is zero.
	DefaultMaxAttempts = 5
	// MaxKindLen matches the CHECK on jobs.kind.
	MaxKindLen = 100
)

// ErrInvalidJob means the job was rejected before touching the database.
var ErrInvalidJob = errors.New("invalid job")

// NewJob describes a job to enqueue.
type NewJob struct {
	// Kind selects the handler (for example "receipt.parse"), 1-100 characters.
	Kind string
	// Payload is marshaled to JSON and must produce a JSON object. nil means {}.
	// Put ids in it, not data the handler can look up: payloads are copied into
	// every retry, log line and dead-letter row.
	Payload any
	// RunAt is when the job becomes eligible. Zero means now (by the database's clock).
	RunAt time.Time
	// MaxAttempts is the total number of runs allowed, including the first.
	// Zero means DefaultMaxAttempts.
	MaxAttempts int
}

// Enqueue inserts a queued job in tx and returns its id. The job exists for
// other transactions only if and when tx commits.
func Enqueue(ctx context.Context, tx pgx.Tx, j NewJob) (uuid.UUID, error) {
	payload, maxAttempts, err := prepare(j)
	if err != nil {
		return uuid.Nil, err
	}
	arg := db.InsertJobParams{Kind: j.Kind, Payload: payload, MaxAttempts: maxAttempts}
	if !j.RunAt.IsZero() {
		arg.RunAt = &j.RunAt
	}
	row, err := db.New(tx).InsertJob(ctx, arg)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert job: %w", err)
	}
	return row.ID, nil
}

// prepare validates j and returns its payload JSON and effective max attempts.
func prepare(j NewJob) (payload []byte, maxAttempts int32, err error) {
	if l := len(j.Kind); l < 1 || l > MaxKindLen {
		return nil, 0, fmt.Errorf("%w: kind must be 1-%d bytes, got %d", ErrInvalidJob, MaxKindLen, l)
	}
	if j.MaxAttempts < 0 || j.MaxAttempts > math.MaxInt32 {
		return nil, 0, fmt.Errorf("%w: max attempts out of range", ErrInvalidJob)
	}
	maxAttempts = int32(j.MaxAttempts)
	if maxAttempts == 0 {
		maxAttempts = DefaultMaxAttempts
	}

	payload = []byte(`{}`)
	if j.Payload != nil {
		if payload, err = json.Marshal(j.Payload); err != nil {
			return nil, 0, fmt.Errorf("%w: payload: %v", ErrInvalidJob, err)
		}
		if !bytes.HasPrefix(payload, []byte("{")) {
			return nil, 0, fmt.Errorf("%w: payload must be a JSON object", ErrInvalidJob)
		}
	}
	return payload, maxAttempts, nil
}

// PeriodicJob is work that should happen once per period, however many workers
// are running.
type PeriodicJob struct {
	Kind        string
	Every       time.Duration // the period, at least one second
	Payload     any           // as NewJob.Payload
	MaxAttempts int           // as NewJob.MaxAttempts
}

// EnqueuePeriodic makes sure a job exists for the current period of p, and
// reports whether this call created it. The period is floor(now / Every) on the
// database clock, so any number of workers calling this, in any order, produce
// exactly one job per period; a period whose job was lost or dead-lettered is not
// repaired, the next period simply gets a new one.
//
// It takes the pool: there is no row to be atomic with, and it is safe to repeat.
func EnqueuePeriodic(ctx context.Context, pool *pgxpool.Pool, p PeriodicJob) (created bool, err error) {
	if p.Every < time.Second {
		return false, fmt.Errorf("%w: period must be at least one second", ErrInvalidJob)
	}
	payload, maxAttempts, err := prepare(NewJob{Kind: p.Kind, Payload: p.Payload, MaxAttempts: p.MaxAttempts})
	if err != nil {
		return false, err
	}
	_, err = db.New(pool).InsertPeriodicJob(ctx, db.InsertPeriodicJobParams{
		Kind: p.Kind, Payload: payload, MaxAttempts: maxAttempts, EverySeconds: p.Every.Seconds(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // this period already has its job
	}
	if err != nil {
		return false, fmt.Errorf("insert periodic job: %w", err)
	}
	return true, nil
}
