package worker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/db"
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
