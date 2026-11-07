package rls_test

import (
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/dbscope"
)

// newCode inserts a join code into group g with the given settings and returns its hash.
func (w *world) newCode(t *testing.T, g grp, maxUses, uses int, expires string, revoked bool) []byte {
	t.Helper()
	h := make([]byte, 32)
	_, _ = rand.Read(h)
	revokedAt := "NULL"
	if revoked {
		revokedAt = "now()"
	}
	w.exec(t, `INSERT INTO join_codes (group_id, code_hash, created_by, max_uses, uses, expires_at, revoked_at)
	           VALUES ($1, $2, $3, $4, $5, now() + $6::interval, `+revokedAt+`)`, g.id, h, g.m1, maxUses, uses, expires)
	return h
}

// redeem calls redeem_join_code as user in a transaction of its own and commits it.
func (w *world) redeem(user uuid.UUID, hash []byte) (db.RedeemedJoinCode, error) {
	tx, err := dbscope.New(w.app).Begin(ctx, user)
	if err != nil {
		return db.RedeemedJoinCode{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	r, err := db.New(tx).RedeemJoinCode(ctx, hash)
	if err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

func (w *world) uses(t *testing.T, hash []byte) int {
	t.Helper()
	var n int
	must(t, w.owner.QueryRow(ctx, `SELECT uses FROM join_codes WHERE code_hash = $1`, hash).Scan(&n))
	return n
}

func (w *world) isMember(t *testing.T, g grp, u uuid.UUID) bool {
	t.Helper()
	var n int
	must(t, w.owner.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE group_id = $1 AND user_id = $2`, g.id, u).Scan(&n))
	return n == 1
}

// A usable code adds the caller once and spends one use; redeeming again is harmless and
// spends nothing.
func TestRedeemAddsTheCallerOnceAndSpendsOneUse(t *testing.T) {
	w := shared(t)
	hash := w.newCode(t, w.A, 5, 0, "1 hour", false)
	u := w.user(t)
	if w.isMember(t, w.A, u) {
		t.Fatal("the user is a member before redeeming")
	}

	r, err := w.redeem(u, hash)
	must(t, err)
	if r.GroupID != w.A.id || !r.Added {
		t.Fatalf("first redeem = %+v, want group A and added", r)
	}
	if !w.isMember(t, w.A, u) || w.uses(t, hash) != 1 {
		t.Fatalf("member=%v uses=%d, want a member and 1 use", w.isMember(t, w.A, u), w.uses(t, hash))
	}
	// The new member now sees the group's rows.
	var n int
	must(t, w.as(t, u).QueryRow(ctx, `SELECT count(*) FROM expenses`).Scan(&n))
	if n == 0 {
		t.Fatal("the new member sees no expenses")
	}

	r, err = w.redeem(u, hash)
	must(t, err)
	if r.Added || r.GroupID != w.A.id {
		t.Fatalf("second redeem = %+v, want group A and not added", r)
	}
	if w.uses(t, hash) != 1 {
		t.Fatalf("uses = %d after redeeming twice, want 1", w.uses(t, hash))
	}
}

// Not found, revoked, expired and exhausted are the same answer: no row.
func TestRedeemRefusesUnusableCodesWithTheSameAnswer(t *testing.T) {
	w := shared(t)
	unknown := make([]byte, 32)
	_, _ = rand.Read(unknown)
	for name, hash := range map[string][]byte{
		"unknown":   unknown,
		"revoked":   w.newCode(t, w.A, 5, 0, "1 hour", true),
		"expired":   w.newCode(t, w.A, 5, 0, "-1 hour", false),
		"exhausted": w.newCode(t, w.A, 1, 1, "1 hour", false),
	} {
		t.Run(name, func(t *testing.T) {
			u := w.user(t)
			_, err := w.redeem(u, hash)
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("got %v, want no rows", err)
			}
			if w.isMember(t, w.A, u) {
				t.Fatal("the user was added")
			}
		})
	}
	// The control for the three above: the same call with a usable code works (it is the
	// previous test), so "no rows" is about the code and not about the call.
}

// There is no way to redeem for someone else, or for nobody.
func TestRedeemNeedsACallerAndCannotBeAskedToJoinSomeoneElse(t *testing.T) {
	w := shared(t)
	hash := w.newCode(t, w.A, 5, 0, "1 hour", false)

	// No identity (a query that escaped dbscope): an error, not a silent no-op.
	_, err := db.New(w.app).RedeemJoinCode(ctx, hash)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "28000" {
		t.Fatalf("with no identity: got %v, want SQLSTATE 28000", err)
	}
	if w.uses(t, hash) != 0 {
		t.Fatal("a use was spent without a caller")
	}

	// The function takes the hash and nothing else.
	var sig string
	must(t, w.owner.QueryRow(ctx, `SELECT pg_get_function_arguments('redeem_join_code(bytea)'::regprocedure)`).Scan(&sig))
	if sig != "p_code_hash bytea" {
		t.Fatalf("redeem_join_code takes (%s): any user-id parameter would let a caller join someone else", sig)
	}

	// The app role cannot look a code up by its hash, which is what a guesser would need.
	var n int
	must(t, w.as(t, w.x).QueryRow(ctx, `SELECT count(*) FROM join_codes WHERE code_hash = $1`, hash).Scan(&n))
	if n != 0 {
		t.Fatalf("a user outside the group can read a join code row: %d", n)
	}
}

// Twenty users race for the last use of a code: exactly one gets in.
func TestRedeemTheLastUseGoesToExactlyOneRacer(t *testing.T) {
	w := shared(t)
	for round := 0; round < 5; round++ {
		hash := w.newCode(t, w.A, 1, 0, "1 hour", false)
		const n = 20
		users := make([]uuid.UUID, n)
		for i := range users {
			users[i] = w.user(t)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]error, n)
		added := make([]bool, n)
		for i := range users {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := w.redeem(users[i], hash)
				results[i], added[i] = err, r.Added
			}()
		}
		close(start)
		wg.Wait()

		wins, misses := 0, 0
		for i, err := range results {
			switch {
			case err == nil && added[i]:
				wins++
			case errors.Is(err, pgx.ErrNoRows):
				misses++
			default:
				t.Fatalf("round %d user %d: err=%v added=%v", round, i, err, added[i])
			}
		}
		if wins != 1 || misses != n-1 || w.uses(t, hash) != 1 {
			t.Fatalf("round %d: %d winners, %d misses, uses=%d; want 1, %d and 1", round, wins, misses, w.uses(t, hash), n-1)
		}
		members := 0
		for _, u := range users {
			if w.isMember(t, w.A, u) {
				members++
			}
		}
		if members != 1 {
			t.Fatalf("round %d: %d of the racers became members, want 1", round, members)
		}
	}
	_ = time.Now
}
