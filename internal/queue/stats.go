package queue

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
)

// KindStats is the state of the unfinished jobs of one kind. See the QueueStats
// query for the exact definitions; the fields are disjoint.
type KindStats struct {
	Kind    string
	Ready   int64 // queued and due
	Delayed int64 // queued for later
	Running int64 // running with a live lease
	Expired int64 // running, lease run out, waiting for takeover or the reaper
	Dead    int64 // dead-lettered
	// OldestReadyAge is how long the longest-waiting ready job has been due; zero
	// when none is ready.
	OldestReadyAge time.Duration
}

// Stats returns one snapshot of every kind that has unfinished jobs, plus an
// all-zero entry for each of alwaysKinds that has none, so a quiet kind still
// reports (a series that vanishes when idle is hard to alert on). Sorted by kind.
// It reads only the small partial indexes, so its cost does not grow with the
// number of finished jobs.
func Stats(ctx context.Context, pool *pgxpool.Pool, alwaysKinds []string) ([]KindStats, error) {
	rows, err := db.New(pool).QueueStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("queue stats: %w", err)
	}
	byKind := make(map[string]KindStats, len(rows)+len(alwaysKinds))
	for _, r := range rows {
		byKind[r.Kind] = KindStats{
			Kind: r.Kind, Ready: r.Ready, Delayed: r.Delayed, Running: r.Running, Expired: r.Expired, Dead: r.Dead,
			OldestReadyAge: time.Duration(r.OldestReadyAgeSeconds * float64(time.Second)),
		}
	}
	for _, k := range alwaysKinds {
		if _, ok := byKind[k]; !ok {
			byKind[k] = KindStats{Kind: k}
		}
	}
	out := make([]KindStats, 0, len(byKind))
	for _, s := range byKind {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}
