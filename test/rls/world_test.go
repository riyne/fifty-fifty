// Package rls_test is the evidence for row-level security (docs/EVIDENCE.md, claim 15): with the
// policies of migration 00016 applied, the application role, acting for one user, cannot
// see or change another group's rows even with a query that has no membership check of
// its own. It runs on a scratch database (created, migrated, dropped), so the counts are
// exact. The BOLA suite passing is NOT evidence for this: the application's own check
// refuses first.
package rls_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/dbscope"
	"github.com/riyne/fifty-fifty/internal/harness/dbrole"
	"github.com/riyne/fifty-fifty/internal/harness/settlerig"
	"github.com/riyne/fifty-fifty/internal/harness/testutil"
)

var ctx = context.Background()

// grp is one group with everything seeded in it.
type grp struct {
	id, m1, m2 uuid.UUID // m1 owns the group and pays; m2 is a member
	exp1       uuid.UUID // finalized, posted to the ledger
	exp2       uuid.UUID // claiming; item2a claimed by m1, item2b unclaimed
	exp3       uuid.UUID // parsing, with a receipt
	exp4       uuid.UUID // finalized, not posted yet
	item1a     uuid.UUID
	item2a     uuid.UUID
	item2b     uuid.UUID
	tx         uuid.UUID // the ledger posting of exp1
	settlement uuid.UUID
	receipt    uuid.UUID
	codeID     uuid.UUID
	codeHash   []byte
}

type world struct {
	sc                      *settlerig.Scratch
	app, owner, worker, sys *pgxpool.Pool
	A, B                    grp
	x                       uuid.UUID // a user who belongs to no group
}

var (
	mainOnce sync.Once
	mainW    *world
)

// TestMain drops the shared scratch database at the end.
func TestMain(m *testing.M) {
	code := m.Run()
	if mainW != nil {
		mainW.close()
	}
	os.Exit(code)
}

// newWorld creates a scratch database with every migration applied and seeds two groups.
func newWorld(t *testing.T) *world {
	t.Helper()
	testutil.Pool(t) // skips, or fails under REQUIRE_DATABASE=1, when there is no database
	ownerURL, appURL := os.Getenv("TEST_OWNER_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		t.Skip("TEST_OWNER_DATABASE_URL and TEST_DATABASE_URL are required")
	}
	sc, err := settlerig.NewScratch(ctx, ownerURL, appURL)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{sc: sc, app: sc.Pool, owner: sc.Owner}
	// These tests ask whether the POLICIES refuse a statement, so fifty_app gets back, in this
	// scratch database only, the privileges migration 00017 took from it (the state of 00004
	// and 00005: the ledger and the audit log stay append-only). With them removed the
	// statements would be refused one layer earlier, by the grant, and the policy would never be
	// asked: the defense in depth would be untested. What the real role may do
	// is test/dbinvariants/roles_test.go.
	for _, g := range []string{
		`GRANT INSERT, UPDATE, DELETE ON users TO fifty_app`,
		`GRANT UPDATE ON claims TO fifty_app`,
		`GRANT UPDATE, DELETE ON jobs, memberships, receipts, settlements TO fifty_app`,
		`GRANT DELETE ON expenses, groups, idempotency_keys, join_codes, receipt_quota TO fifty_app`,
	} {
		if _, err := sc.Owner.Exec(ctx, g); err != nil {
			t.Fatalf("%s: %v", g, err)
		}
	}
	if w.worker, err = pgxpool.New(ctx, sc.WorkerURL); err != nil {
		t.Fatal(err)
	}
	if w.sys, err = pgxpool.New(ctx, sc.SystemURL); err != nil {
		t.Fatal(err)
	}
	w.x = w.user(t)
	w.A = w.seedGroup(t, "A")
	w.B = w.seedGroup(t, "B")
	return w
}

// shared returns the world used by the tests that only read or write inside rolled-back
// transactions or in their own groups.
func shared(t *testing.T) *world {
	t.Helper()
	testutil.Pool(t) // a skip (no database) applies to every test, not only the first
	mainOnce.Do(func() {
		// newWorld needs a testing.T; a failure is reported by whoever asked first.
		mainW = newWorld(t)
	})
	if mainW == nil {
		t.Fatal("the shared world could not be created")
	}
	return mainW
}

func (w *world) close() {
	w.worker.Close()
	w.sys.Close()
	_ = w.sc.Drop(context.Background())
}

func (w *world) user(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	must(t, w.owner.QueryRow(ctx, `INSERT INTO users (issuer, subject) VALUES ('rls', $1) RETURNING id`, uuid.NewString()).Scan(&id))
	return id
}

func (w *world) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := w.owner.Exec(ctx, sql, args...)
	must(t, err)
}

func (w *world) seedGroup(t *testing.T, name string) grp {
	t.Helper()
	g := grp{m1: w.user(t), m2: w.user(t)}
	must(t, w.owner.QueryRow(ctx, `INSERT INTO groups (name, currency, created_by) VALUES ($1, 'CAD', $2) RETURNING id`, name, g.m1).Scan(&g.id))
	w.exec(t, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, g.id, g.m1, g.m2)

	expense := func(status string) uuid.UUID {
		var id uuid.UUID
		q := `INSERT INTO expenses (group_id, payer_id, description, status) VALUES ($1, $2, 'dinner', $3) RETURNING id`
		if status == "finalized" {
			q = `INSERT INTO expenses (group_id, payer_id, description, status, finalized_at) VALUES ($1, $2, 'dinner', $3, now()) RETURNING id`
		}
		must(t, w.owner.QueryRow(ctx, q, g.id, g.m1, status).Scan(&id))
		return id
	}
	item := func(exp uuid.UUID, pos int) uuid.UUID {
		var id uuid.UUID
		must(t, w.owner.QueryRow(ctx,
			`INSERT INTO expense_items (expense_id, position, description, price_minor) VALUES ($1, $2, 'item', 1000) RETURNING id`, exp, pos).Scan(&id))
		return id
	}
	g.exp1, g.exp2, g.exp3, g.exp4 = expense("finalized"), expense("claiming"), expense("parsing"), expense("finalized")
	g.item1a = item(g.exp1, 0)
	item(g.exp1, 1)
	g.item2a, g.item2b = item(g.exp2, 0), item(g.exp2, 1)
	claim := func(it, exp, member uuid.UUID) {
		w.exec(t, `INSERT INTO claims (item_id, expense_id, group_id, member_id) VALUES ($1, $2, $3, $4)`, it, exp, g.id, member)
	}
	claim(g.item1a, g.exp1, g.m1)
	claim(g.item1a, g.exp1, g.m2)
	claim(g.item2a, g.exp2, g.m1)

	// A ledger posting: header and entries in one transaction, balanced.
	dbtx, err := w.owner.Begin(ctx)
	must(t, err)
	must(t, dbtx.QueryRow(ctx,
		`INSERT INTO ledger_transactions (group_id, kind, expense_id, created_by) VALUES ($1, 'expense', $2, $3) RETURNING id`,
		g.id, g.exp1, g.m1).Scan(&g.tx))
	for _, e := range []struct {
		m uuid.UUID
		a int64
	}{{g.m1, 500}, {g.m2, -500}} {
		_, err := dbtx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor) VALUES ($1, $2, $3, $4)`, g.tx, g.id, e.m, e.a)
		must(t, err)
	}
	must(t, dbtx.Commit(ctx))

	must(t, w.owner.QueryRow(ctx,
		`INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 100) RETURNING id`, g.id, g.m2, g.m1).Scan(&g.settlement))
	must(t, w.owner.QueryRow(ctx,
		`INSERT INTO receipts (expense_id, group_id, uploader_id, object_key, content_type, size_bytes, width_px, height_px)
		 VALUES ($1, $2, $3, repeat('a', 64), 'image/png', 10, 10, 10) RETURNING id`, g.exp3, g.id, g.m1).Scan(&g.receipt))
	w.exec(t, `INSERT INTO audit_events (actor_user_id, group_id, kind) VALUES ($1, $2, 'seed')`, g.m1, g.id)

	g.codeHash = make([]byte, 32)
	_, _ = rand.Read(g.codeHash)
	must(t, w.owner.QueryRow(ctx,
		`INSERT INTO join_codes (group_id, code_hash, created_by, max_uses, expires_at) VALUES ($1, $2, $3, 5, now() + interval '1 day') RETURNING id`,
		g.id, g.codeHash, g.m1).Scan(&g.codeID))

	for _, m := range []uuid.UUID{g.m1, g.m2} {
		w.exec(t, `INSERT INTO idempotency_keys (user_id, key, fingerprint, response_status, response_body, expires_at)
		           VALUES ($1, 'seed', '\x01', 200, '\x7b7d', now() + interval '1 day')`, m)
		w.exec(t, `INSERT INTO receipt_quota (user_id, day, used) VALUES ($1, current_date, 1)`, m)
	}
	return g
}

// as opens a transaction of the application role acting for user, rolled back at cleanup.
func (w *world) as(t *testing.T, user uuid.UUID) pgx.Tx {
	t.Helper()
	tx, err := dbscope.New(w.app).Begin(ctx, user)
	must(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// rlsRefused reports whether err is Postgres refusing a row because of a policy (and not,
// say, a revoked privilege, a constraint or a trigger, which have other messages).
func rlsRefused(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "42501" && strings.Contains(pg.Message, "row-level security")
}

// privilegeRefused reports a revoked privilege: SQLSTATE 42501, "permission denied".
func privilegeRefused(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "42501" && strings.Contains(pg.Message, "permission denied")
}

var _ = dbrole.App // the role names are used by the catalog tests
