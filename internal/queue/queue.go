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
	if l := len(j.Kind); l < 1 || l > MaxKindLen {
		return uuid.Nil, fmt.Errorf("%w: kind must be 1-%d bytes, got %d", ErrInvalidJob, MaxKindLen, l)
	}
	if j.MaxAttempts < 0 || j.MaxAttempts > math.MaxInt32 {
		return uuid.Nil, fmt.Errorf("%w: max attempts out of range", ErrInvalidJob)
	}
	maxAttempts := j.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = DefaultMaxAttempts
	}

	payload := []byte(`{}`)
	if j.Payload != nil {
		var err error
		if payload, err = json.Marshal(j.Payload); err != nil {
			return uuid.Nil, fmt.Errorf("%w: payload: %v", ErrInvalidJob, err)
		}
		if !bytes.HasPrefix(payload, []byte("{")) {
			return uuid.Nil, fmt.Errorf("%w: payload must be a JSON object", ErrInvalidJob)
		}
	}

	arg := db.InsertJobParams{Kind: j.Kind, Payload: payload, MaxAttempts: int32(maxAttempts)}
	if !j.RunAt.IsZero() {
		arg.RunAt = &j.RunAt
	}
	row, err := db.New(tx).InsertJob(ctx, arg)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert job: %w", err)
	}
	return row.ID, nil
}
