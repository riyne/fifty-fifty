package idempotency_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/dbscope"
	"github.com/riyne/fifty-fifty/internal/idempotency"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// The helpers below seed and inspect as the owner, which sees every row whatever
// row-level security does to the roles under test; Do itself runs on the app pool and
// DeleteExpired on the worker pool, the roles they run as.
func newUser(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := db.New(testutil.SharedOwnerPool(t)).EnsureUser(context.Background(), db.EnsureUserParams{
		Issuer: "https://idempotency-test.example", Subject: testutil.UniqueSubject("u"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func keyRows(t *testing.T, user uuid.UUID, key string) int {
	t.Helper()
	var n int
	err := testutil.SharedOwnerPool(t).QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys WHERE user_id = $1 AND key = $2`, user, key).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var fp = idempotency.Fingerprint("POST", "/x", "body")

func TestDo_RunsOnceThenReplays(t *testing.T) {
	pool := testutil.Pool(t)
	user, key := newUser(t), uuid.NewString()
	calls := 0
	fn := func(context.Context, pgx.Tx) (idempotency.Result, error) {
		calls++
		return idempotency.Result{Status: 201, Body: []byte(`{"n":1}`)}, nil
	}

	first, err := idempotency.Do(context.Background(), dbscope.New(pool), user, key, fp, fn)
	if err != nil || first.Replayed || first.Status != 201 {
		t.Fatalf("first: %+v, %v", first, err)
	}
	second, err := idempotency.Do(context.Background(), dbscope.New(pool), user, key, fp, fn)
	if err != nil || !second.Replayed || second.Status != 201 || string(second.Body) != `{"n":1}` {
		t.Fatalf("second: %+v, %v", second, err)
	}
	if calls != 1 {
		t.Fatalf("fn ran %d times, want 1", calls)
	}
}

func TestDo_StoresClientErrorOutcomes(t *testing.T) {
	pool := testutil.Pool(t)
	user, key := newUser(t), uuid.NewString()
	calls := 0
	fn := func(context.Context, pgx.Tx) (idempotency.Result, error) {
		calls++
		return idempotency.Result{Status: 404, Body: []byte(`{"code":"nope"}`)}, nil
	}
	for range 3 {
		if _, err := idempotency.Do(context.Background(), dbscope.New(pool), user, key, fp, fn); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("fn ran %d times, want 1: a 404 outcome is final for its key", calls)
	}
}

func TestDo_ServerErrorsAndFailuresAreNotStored(t *testing.T) {
	pool := testutil.Pool(t)
	user := newUser(t)

	t.Run("5xx result", func(t *testing.T) {
		key := uuid.NewString()
		calls := 0
		fn := func(context.Context, pgx.Tx) (idempotency.Result, error) {
			calls++
			return idempotency.Result{Status: 503, Body: []byte(`{}`)}, nil
		}
		for range 2 {
			if _, err := idempotency.Do(context.Background(), dbscope.New(pool), user, key, fp, fn); err != nil {
				t.Fatal(err)
			}
		}
		if calls != 2 || keyRows(t, user, key) != 0 {
			t.Fatalf("calls=%d rows=%d, want a retry to re-run and nothing stored", calls, keyRows(t, user, key))
		}
	})

	t.Run("error", func(t *testing.T) {
		key := uuid.NewString()
		boom := errors.New("boom")
		_, err := idempotency.Do(context.Background(), dbscope.New(pool), user, key, fp, func(context.Context, pgx.Tx) (idempotency.Result, error) {
			return idempotency.Result{}, boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if keyRows(t, user, key) != 0 {
			t.Fatal("a failed attempt left a key row behind")
		}
		out, err := idempotency.Do(context.Background(), dbscope.New(pool), user, key, fp, func(context.Context, pgx.Tx) (idempotency.Result, error) {
			return idempotency.Result{Status: 200, Body: []byte(`{}`)}, nil
		})
		if err != nil || out.Replayed {
			t.Fatalf("retry after failure: %+v, %v", out, err)
		}
	})
}

// fn's own writes must roll back with the claim when fn fails.
func TestDo_EffectsCommitOrRollBackWithTheKey(t *testing.T) {
	pool := testutil.Pool(t)
	owner := testutil.SharedOwnerPool(t)
	user := newUser(t)
	count := func() int {
		var n int
		if err := owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE actor_user_id = $1`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// audit_events needs a group; make one.
	g, err := db.New(owner).CreateGroup(context.Background(), db.CreateGroupParams{Name: "g", Currency: "CAD", CreatedBy: user})
	if err != nil {
		t.Fatal(err)
	}
	// The user must belong to the group the audit event is in: row-level security
	// refuses an event for a group the caller is not in.
	if err := db.New(owner).AddMembership(context.Background(), db.AddMembershipParams{GroupID: g.ID, UserID: user, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	write := func(ctx context.Context, tx pgx.Tx) error {
		return db.New(tx).InsertAuditEvent(ctx, db.InsertAuditEventParams{ActorUserID: user, GroupID: g.ID, Kind: "t", Data: []byte(`{}`)})
	}

	_, err = idempotency.Do(context.Background(), dbscope.New(pool), user, uuid.NewString(), fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		if err := write(ctx, tx); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{}, errors.New("fail after writing")
	})
	if err == nil || count() != 0 {
		t.Fatalf("effects survived a failed attempt: err=%v rows=%d", err, count())
	}

	_, err = idempotency.Do(context.Background(), dbscope.New(pool), user, uuid.NewString(), fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return idempotency.Result{Status: 200, Body: []byte(`{}`)}, write(ctx, tx)
	})
	if err != nil || count() != 1 {
		t.Fatalf("effects missing after success: err=%v rows=%d", err, count())
	}
}

func TestDo_FingerprintMismatch(t *testing.T) {
	pool := testutil.Pool(t)
	user, key := newUser(t), uuid.NewString()
	ok := func(context.Context, pgx.Tx) (idempotency.Result, error) {
		return idempotency.Result{Status: 200, Body: []byte(`{}`)}, nil
	}
	if _, err := idempotency.Do(context.Background(), dbscope.New(pool), user, key, fp, ok); err != nil {
		t.Fatal(err)
	}
	_, err := idempotency.Do(context.Background(), dbscope.New(pool), user, key, idempotency.Fingerprint("POST", "/x", "other"), ok)
	if !errors.Is(err, idempotency.ErrKeyReuse) {
		t.Fatalf("err = %v, want ErrKeyReuse", err)
	}
}

func TestFingerprint_PartsAreUnambiguous(t *testing.T) {
	if string(idempotency.Fingerprint("ab", "c")) == string(idempotency.Fingerprint("a", "bc")) {
		t.Fatal(`("ab","c") and ("a","bc") collide`)
	}
}

func TestParseKey(t *testing.T) {
	cases := map[string]bool{
		"":                           false,
		"3f2b8f0e-6a52-4a3e-9d0e-1c": true,
		"a b":                        false,
		"tab\there":                  false,
		"é":                          false,
		strings.Repeat("k", 255):     true,
		strings.Repeat("k", 256):     false,
		"!~":                         true,
	}
	for key, valid := range cases {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if key != "" {
			r.Header["Idempotency-Key"] = []string{key} // bypass header canonicalization/validation
		}
		_, err := idempotency.ParseKey(r)
		if (err == nil) != valid {
			t.Errorf("ParseKey(%q): err=%v, valid=%v", key, err, valid)
		}
	}
}

// insertKey writes an idempotency row directly, expiring `age` from now (negative:
// already expired).
func insertKey(t *testing.T, user uuid.UUID, key string, in string) {
	t.Helper()
	_, err := testutil.SharedOwnerPool(t).Exec(context.Background(),
		`INSERT INTO idempotency_keys (user_id, key, fingerprint, response_status, response_body, expires_at)
		 VALUES ($1, $2, '\x01', 200, '\x7b7d', now() + $3::interval)`, user, key, in)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeleteExpired_RemovesOnlyExpiredKeysAndRespectsTheBatch(t *testing.T) {
	worker := testutil.WorkerPool(t)
	user := newUser(t)
	var expired, live []string
	for i := range 7 {
		k := uuid.NewString()
		// Older than anything another test leaves behind, so these sort first and
		// the batch below is made of them.
		insertKey(t, user, k, "-10 years -"+string(rune('0'+i))+" seconds")
		expired = append(expired, k)
	}
	for range 3 {
		k := uuid.NewString()
		insertKey(t, user, k, "1 hour")
		live = append(live, k)
	}

	// The cleanup is global and the table shared, so another test's cleanup running
	// at the same moment can take some of these rows: assert what holds either way.
	n, err := idempotency.DeleteExpired(context.Background(), worker, 3)
	if err != nil || n > 3 {
		t.Fatalf("deleted %d, %v; one call must delete at most its batch of 3", n, err)
	}
	for { // until a call deletes nothing, as the worker does
		n, err := idempotency.DeleteExpired(context.Background(), worker, 3)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	for _, k := range expired {
		if keyRows(t, user, k) != 0 {
			t.Errorf("expired key %s survived", k)
		}
	}
	for _, k := range live {
		if keyRows(t, user, k) != 1 {
			t.Errorf("live key %s was deleted", k)
		}
	}
	for _, bad := range []int{0, -1} {
		if _, err := idempotency.DeleteExpired(context.Background(), worker, bad); err == nil {
			t.Errorf("batch %d accepted", bad)
		}
	}
}

// A request re-claims an expired key (Do's takeover) while the cleanup is deleting
// it. The cleanup must wait for that request and then leave the now-live key alone;
// deleting it would let the request's duplicate run twice.
func TestDeleteExpired_DoesNotDeleteAKeyReclaimedInFlight(t *testing.T) {
	app, worker := testutil.Pool(t), testutil.WorkerPool(t)
	ctx := context.Background()
	user, key := newUser(t), uuid.NewString()
	insertKey(t, user, key, "-10 years")

	// The request that takes the key over runs as the app role, as the user.
	tx, err := dbscope.New(app).Begin(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := db.New(tx).ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{UserID: user, Key: key, Fingerprint: fp}); err != nil {
		t.Fatalf("takeover of the expired key: %v", err)
	}

	done := make(chan error, 1)
	go func() { _, err := idempotency.DeleteExpired(ctx, worker, 1000); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("the cleanup finished (%v) without waiting for the in-flight claim of the row it was about to delete", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if keyRows(t, user, key) != 1 {
		t.Fatal("the cleanup deleted a key that a request had just re-claimed")
	}
}
