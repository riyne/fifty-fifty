package idempotency_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/idempotency"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

func newUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	u, err := db.New(pool).EnsureUser(context.Background(), db.EnsureUserParams{
		Issuer: "https://idempotency-test.example", Subject: testutil.UniqueSubject("u"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func keyRows(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, key string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys WHERE user_id = $1 AND key = $2`, user, key).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var fp = idempotency.Fingerprint("POST", "/x", "body")

func TestDo_RunsOnceThenReplays(t *testing.T) {
	pool := testutil.Pool(t)
	user, key := newUser(t, pool), uuid.NewString()
	calls := 0
	fn := func(context.Context, pgx.Tx) (idempotency.Result, error) {
		calls++
		return idempotency.Result{Status: 201, Body: []byte(`{"n":1}`)}, nil
	}

	first, err := idempotency.Do(context.Background(), pool, user, key, fp, fn)
	if err != nil || first.Replayed || first.Status != 201 {
		t.Fatalf("first: %+v, %v", first, err)
	}
	second, err := idempotency.Do(context.Background(), pool, user, key, fp, fn)
	if err != nil || !second.Replayed || second.Status != 201 || string(second.Body) != `{"n":1}` {
		t.Fatalf("second: %+v, %v", second, err)
	}
	if calls != 1 {
		t.Fatalf("fn ran %d times, want 1", calls)
	}
}

func TestDo_StoresClientErrorOutcomes(t *testing.T) {
	pool := testutil.Pool(t)
	user, key := newUser(t, pool), uuid.NewString()
	calls := 0
	fn := func(context.Context, pgx.Tx) (idempotency.Result, error) {
		calls++
		return idempotency.Result{Status: 404, Body: []byte(`{"code":"nope"}`)}, nil
	}
	for range 3 {
		if _, err := idempotency.Do(context.Background(), pool, user, key, fp, fn); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("fn ran %d times, want 1: a 404 outcome is final for its key", calls)
	}
}

func TestDo_ServerErrorsAndFailuresAreNotStored(t *testing.T) {
	pool := testutil.Pool(t)
	user := newUser(t, pool)

	t.Run("5xx result", func(t *testing.T) {
		key := uuid.NewString()
		calls := 0
		fn := func(context.Context, pgx.Tx) (idempotency.Result, error) {
			calls++
			return idempotency.Result{Status: 503, Body: []byte(`{}`)}, nil
		}
		for range 2 {
			if _, err := idempotency.Do(context.Background(), pool, user, key, fp, fn); err != nil {
				t.Fatal(err)
			}
		}
		if calls != 2 || keyRows(t, pool, user, key) != 0 {
			t.Fatalf("calls=%d rows=%d, want a retry to re-run and nothing stored", calls, keyRows(t, pool, user, key))
		}
	})

	t.Run("error", func(t *testing.T) {
		key := uuid.NewString()
		boom := errors.New("boom")
		_, err := idempotency.Do(context.Background(), pool, user, key, fp, func(context.Context, pgx.Tx) (idempotency.Result, error) {
			return idempotency.Result{}, boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if keyRows(t, pool, user, key) != 0 {
			t.Fatal("a failed attempt left a key row behind")
		}
		out, err := idempotency.Do(context.Background(), pool, user, key, fp, func(context.Context, pgx.Tx) (idempotency.Result, error) {
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
	user := newUser(t, pool)
	count := func() int {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE actor_user_id = $1`, user).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// audit_events needs a group; make one.
	g, err := db.New(pool).CreateGroup(context.Background(), db.CreateGroupParams{Name: "g", Currency: "CAD", CreatedBy: user})
	if err != nil {
		t.Fatal(err)
	}
	write := func(ctx context.Context, tx pgx.Tx) error {
		return db.New(tx).InsertAuditEvent(ctx, db.InsertAuditEventParams{ActorUserID: user, GroupID: g.ID, Kind: "t", Data: []byte(`{}`)})
	}

	_, err = idempotency.Do(context.Background(), pool, user, uuid.NewString(), fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		if err := write(ctx, tx); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{}, errors.New("fail after writing")
	})
	if err == nil || count() != 0 {
		t.Fatalf("effects survived a failed attempt: err=%v rows=%d", err, count())
	}

	_, err = idempotency.Do(context.Background(), pool, user, uuid.NewString(), fp, func(ctx context.Context, tx pgx.Tx) (idempotency.Result, error) {
		return idempotency.Result{Status: 200, Body: []byte(`{}`)}, write(ctx, tx)
	})
	if err != nil || count() != 1 {
		t.Fatalf("effects missing after success: err=%v rows=%d", err, count())
	}
}

func TestDo_FingerprintMismatch(t *testing.T) {
	pool := testutil.Pool(t)
	user, key := newUser(t, pool), uuid.NewString()
	ok := func(context.Context, pgx.Tx) (idempotency.Result, error) {
		return idempotency.Result{Status: 200, Body: []byte(`{}`)}, nil
	}
	if _, err := idempotency.Do(context.Background(), pool, user, key, fp, ok); err != nil {
		t.Fatal(err)
	}
	_, err := idempotency.Do(context.Background(), pool, user, key, idempotency.Fingerprint("POST", "/x", "other"), ok)
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
