package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
)

// MaxClaimBatch bounds ClaimParams.Limit.
const MaxClaimBatch = 1000

// ClaimParams says what a worker is asking for.
type ClaimParams struct {
	// Kinds the worker has handlers for. A worker must never claim a job it
	// cannot run, so there is no "any kind".
	Kinds []string
	// Limit is the most jobs to claim, 1 to MaxClaimBatch.
	Limit int
	// Lease is how long the worker holds each job before another worker may take
	// it over, measured on the database clock. The worker must finish or
	// heartbeat within it.
	Lease time.Duration
}

// Claimed is a job leased to the caller.
type Claimed struct {
	ID      uuid.UUID
	Kind    string
	Payload []byte
	// Attempt is the number of this run, starting at 1.
	Attempt     int
	MaxAttempts int
	// LeaseToken is the fencing token. Pass it back with every later write about
	// this job; the write only takes effect while it is still the job's token.
	LeaseToken     uuid.UUID
	LeaseExpiresAt time.Time
}

// ClaimJobs leases up to p.Limit eligible jobs to the caller (see the ClaimJobs
// query for the rules). It never blocks on jobs other workers are claiming.
//
// It takes the pool, not a transaction: the claim is one autocommitted statement,
// so the row locks it takes last only as long as the statement and the lease, not
// a lock, is what keeps other workers away while the job runs.
func ClaimJobs(ctx context.Context, pool *pgxpool.Pool, p ClaimParams) ([]Claimed, error) {
	switch {
	case len(p.Kinds) == 0:
		return nil, fmt.Errorf("%w: no kinds to claim", ErrInvalidClaim)
	case p.Limit < 1 || p.Limit > MaxClaimBatch:
		return nil, fmt.Errorf("%w: limit must be 1-%d", ErrInvalidClaim, MaxClaimBatch)
	case p.Lease <= 0:
		return nil, fmt.Errorf("%w: lease must be positive", ErrInvalidClaim)
	}
	rows, err := db.New(pool).ClaimJobs(ctx, db.ClaimJobsParams{
		Kinds:        p.Kinds,
		Batch:        int32(p.Limit),
		LeaseSeconds: p.Lease.Seconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	out := make([]Claimed, len(rows))
	for i, r := range rows {
		// Unreachable: the statement sets both, and the table refuses a running job
		// without them. Check anyway rather than dereference a nil.
		if !r.LeaseToken.Valid || r.LeaseExpiresAt == nil {
			return nil, fmt.Errorf("claim jobs: job %s came back without a lease", r.ID)
		}
		out[i] = Claimed{
			ID: r.ID, Kind: r.Kind, Payload: r.Payload,
			Attempt: int(r.Attempts), MaxAttempts: int(r.MaxAttempts),
			LeaseToken: uuid.UUID(r.LeaseToken.Bytes), LeaseExpiresAt: *r.LeaseExpiresAt,
		}
	}
	return out, nil
}

// ErrInvalidClaim means the claim was rejected before touching the database.
var ErrInvalidClaim = errors.New("invalid claim")

// ReapExhausted moves running jobs whose lease expired on their final attempt to
// dead, and returns their ids. A job that crashes its worker on the last attempt
// leaves no one to report the failure, so something has to notice; the worker
// loop calls this periodically.
func ReapExhausted(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	ids, err := db.New(pool).ReapExhaustedJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("reap exhausted jobs: %w", err)
	}
	return ids, nil
}
