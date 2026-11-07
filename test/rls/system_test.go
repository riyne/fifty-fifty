package rls_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The worker and the api's second pool act for no user. Their policies let them see what
// their grants allow, and the grants (00015) keep them away from the rest. An empty result
// here would mean the worker could not do its job, so each count is compared with the
// owner's and must be non-zero.
func TestWorkerAndSystemSeeEverythingTheirGrantsAllow(t *testing.T) {
	w := shared(t)
	count := func(t *testing.T, p *pgxpool.Pool, table string) (int, error) {
		var n int
		err := p.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		return n, err
	}
	for _, c := range []struct {
		role   string
		pool   *pgxpool.Pool
		tables []string
	}{
		{"fifty_worker", w.worker, []string{"expense_items", "expenses", "groups", "idempotency_keys", "ledger_entries", "ledger_transactions", "receipts", "settlements"}},
		{"fifty_system", w.sys, []string{"groups", "ledger_entries", "ledger_transactions", "settlements", "users"}},
	} {
		for _, table := range c.tables {
			t.Run(c.role+"/"+table, func(t *testing.T) {
				got, err := count(t, c.pool, table)
				must(t, err)
				var want int
				must(t, w.owner.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&want))
				if got != want || want == 0 {
					t.Fatalf("sees %d rows, the owner sees %d (want equal and non-zero)", got, want)
				}
			})
		}
	}

	// And what they have no grant on stays out of reach, whatever the policies say.
	for _, c := range []struct {
		role  string
		pool  *pgxpool.Pool
		table string
	}{
		{"fifty_worker", w.worker, "memberships"},
		{"fifty_worker", w.worker, "users"},
		{"fifty_worker", w.worker, "claims"},
		{"fifty_worker", w.worker, "join_codes"},
		{"fifty_system", w.sys, "expenses"},
		{"fifty_system", w.sys, "memberships"},
		{"fifty_system", w.sys, "receipts"},
		{"fifty_system", w.sys, "join_codes"},
	} {
		if _, err := count(t, c.pool, c.table); !privilegeRefused(err) {
			t.Errorf("%s on %s: want a privilege refusal, got %v", c.role, c.table, err)
		}
	}
}

// audit_events is insert-only for both roles (no SELECT grant): they can record an event for
// any group, which is what Resolve and the parse job do, and cannot read the trail back.
func TestWorkerAndSystemCanAppendAuditEventsToAnyGroup(t *testing.T) {
	w := shared(t)
	for _, c := range []struct {
		role string
		pool *pgxpool.Pool
	}{{"fifty_worker", w.worker}, {"fifty_system", w.sys}} {
		for _, g := range []grp{w.A, w.B} {
			tx, err := c.pool.Begin(ctx)
			must(t, err)
			_, err = tx.Exec(ctx, `INSERT INTO audit_events (actor_user_id, group_id, kind) VALUES ($1, $2, 'x')`, g.m1, g.id)
			_ = tx.Rollback(ctx)
			if err != nil {
				t.Errorf("%s inserting an audit event for a group: %v", c.role, err)
			}
		}
		var n int
		if err := c.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&n); !privilegeRefused(err) {
			t.Errorf("%s reading audit_events: want a privilege refusal, got %v", c.role, err)
		}
	}
}
