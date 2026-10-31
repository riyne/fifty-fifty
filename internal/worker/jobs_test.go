package worker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// deleteAllExpired loops over batches until none are left.
func TestDeleteAllExpired_LoopsOverBatches(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	u, err := db.New(pool).EnsureUser(ctx, db.EnsureUserParams{Issuer: "https://worker-test.example", Subject: testutil.UniqueSubject("c")})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(in string) uuid.UUID {
		k := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, fingerprint, response_status, response_body, expires_at)
			VALUES ($1, $2, '\x01', 200, '\x7b7d', now() + $3::interval)`, u.ID, k.String(), in); err != nil {
			t.Fatal(err)
		}
		return k
	}
	var expired, live []uuid.UUID
	for range 7 {
		expired = append(expired, insert("-1 hour"))
	}
	live = append(live, insert("1 hour"), insert("1 hour"))

	// Seven rows in batches of two: four batches. The table is shared and the
	// cleanup is global, so other tests' cleanups may take some of these rows first;
	// what must hold whoever deletes them is the end state below.
	if _, err := deleteAllExpired(ctx, pool, 2); err != nil {
		t.Fatal(err)
	}
	count := func(ids []uuid.UUID) (n int) {
		for _, k := range ids {
			var c int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1 AND key = $2`, u.ID, k.String()).Scan(&c); err != nil {
				t.Fatal(err)
			}
			n += c
		}
		return n
	}
	if count(expired) != 0 || count(live) != 2 {
		t.Errorf("%d expired and %d live keys remain, want 0 and 2", count(expired), count(live))
	}
}

// Two cleaners can overlap: another one deletes a row this batch had selected, so
// the batch comes back short while rows remain. The loop must carry on. Made
// deterministic with a lock: a transaction holds the oldest row, the first batch
// blocks on it, the transaction deletes it, and the batch finishes one row short.
func TestDeleteAllExpired_ContinuesAfterAShortBatch(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	u, err := db.New(pool).EnsureUser(ctx, db.EnsureUserParams{Issuer: "https://worker-test.example", Subject: testutil.UniqueSubject("short")})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := range 5 { // key 4 is the oldest, then 3, 2, ...: batches of two take 4+3, 2+1, 0
		k := uuid.NewString()
		keys = append(keys, k)
		if _, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, fingerprint, response_status, response_body, expires_at)
			VALUES ($1, $2, '\x01', 200, '\x7b7d', now() - interval '20 years' - make_interval(secs => $3))`, u.ID, k, i); err != nil {
			t.Fatal(err)
		}
	}
	oldest := keys[4]

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT 1 FROM idempotency_keys WHERE user_id = $1 AND key = $2 FOR UPDATE`, u.ID, oldest); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { _, err := deleteAllExpired(ctx, pool, 2); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("the cleanup finished (%v) although its first batch should be waiting on a locked row", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE user_id = $1 AND key = $2`, u.ID, oldest); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1`, u.ID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d expired keys left behind: the cleanup stopped at the short batch", left)
	}
}

func finishedJobAt(t *testing.T, pool *pgxpool.Pool, kind, state, age string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO jobs (kind, payload, state, attempts, max_attempts, run_at, finished_at)
		VALUES ($1, '{}', $2, 1, CASE WHEN $2 = 'dead' THEN 1 ELSE 5 END, now(), now() - $3::interval)
		RETURNING id`, kind, state, age).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func jobGone(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 0
}

// The handler prunes until nothing is left in range, however small the batch, and
// leaves dead-letters and recent jobs alone. Ages are days against a day of
// retention so other tests' fresh jobs are never in range.
func TestPruneAll_LoopsOverBatches(t *testing.T) {
	pool := testutil.Pool(t)
	kind := "test.prune." + uuid.NewString()
	var old []uuid.UUID
	for range 7 {
		old = append(old, finishedJobAt(t, pool, kind, "succeeded", "5 days"))
	}
	recent := finishedJobAt(t, pool, kind, "succeeded", "1 hour")
	dead := finishedJobAt(t, pool, kind, "dead", "5 days")

	if _, err := pruneAll(context.Background(), pool, 24*time.Hour, 2); err != nil {
		t.Fatal(err)
	}
	for _, id := range old {
		if !jobGone(t, pool, id) {
			t.Errorf("job %s past retention survived", id)
		}
	}
	if jobGone(t, pool, recent) || jobGone(t, pool, dead) {
		t.Error("pruned a recent job or a dead-letter")
	}
}

func TestJobsPrune_HandlerRunsThePrune(t *testing.T) {
	pool := testutil.Pool(t)
	kind := "test.prune-handler." + uuid.NewString()
	old := finishedJobAt(t, pool, kind, "succeeded", "9 days")
	keep := finishedJobAt(t, pool, kind, "succeeded", "1 day")

	commit, err := JobsPrune(pool, worker7Days)(context.Background(), queue.Claimed{})
	if err != nil || commit != nil {
		t.Fatalf("handler returned commit=%v err=%v; the prune is all phase one", commit != nil, err)
	}
	if !jobGone(t, pool, old) || jobGone(t, pool, keep) {
		t.Fatal("the handler must delete the 9-day-old job and keep the 1-day-old one")
	}
	if JobRetention != worker7Days {
		t.Fatalf("JobRetention = %v; the handler test assumes the documented 7 days", JobRetention)
	}
	if PruneSchedule.Kind != KindJobsPrune || PruneSchedule.Every != time.Hour {
		t.Fatalf("schedule %+v", PruneSchedule)
	}
}

const worker7Days = 7 * 24 * time.Hour
