package rls_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// FORCE ROW LEVEL SECURITY (set on every policy table by 00016) makes the policies apply to the
// table's OWNER too. In this project the owner is the superuser `fifty`, which bypasses row-level
// security whatever FORCE says, so on the development database the setting has no visible
// effect. It matters wherever the owner is an ordinary role (a managed Postgres, a deployment
// that does not migrate as a superuser): there, without FORCE, the owner would see and change
// every row. This test shows that with a non-superuser owner the policies hold, and that FORCE
// is the reason: with it switched off the same owner sees everything. It runs in one
// transaction on the shared scratch database and rolls it back (ownership changes, the role
// and the ALTERs are all transactional), so nothing is left behind.
func TestForceRowLevelSecurity_BindsANonSuperuserTableOwner(t *testing.T) {
	w := shared(t)
	tx, err := w.owner.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck // the point: nothing persists

	const role = "zz_rls_table_owner"
	_, err = tx.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOBYPASSRLS`)
	must(t, err)

	// Every table that has a policy, with its row count as seen by the superuser.
	rows, err := tx.Query(ctx, `SELECT c.relname FROM pg_class c
		WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r' AND c.relforcerowsecurity ORDER BY 1`)
	must(t, err)
	var tables []string
	for rows.Next() {
		var n string
		must(t, rows.Scan(&n))
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 14 {
		t.Fatalf("only %d tables have FORCE ROW LEVEL SECURITY: %v (00016 sets it on 14)", len(tables), tables)
	}

	count := func(table string) int {
		var n int
		must(t, tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n))
		return n
	}
	asOwner := func(table string) int { // as the non-superuser owner, inside the transaction
		_, err := tx.Exec(ctx, `SET LOCAL ROLE `+role)
		must(t, err)
		defer func() { _, err := tx.Exec(ctx, `RESET ROLE`); must(t, err) }()
		return count(table)
	}

	checked := 0
	for _, table := range tables {
		real := count(table) // as the superuser: bypasses row-level security
		_, err := tx.Exec(ctx, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` OWNER TO `+role)
		must(t, err)
		if real == 0 {
			continue // an empty table cannot show an effect either way
		}
		checked++
		if got := asOwner(table); got != 0 {
			t.Errorf("%s: its non-superuser OWNER sees %d of %d rows: FORCE ROW LEVEL SECURITY is not binding the owner", table, got, real)
		}
		// The control: with FORCE off the same owner sees every row. Without this, "0 rows"
		// could just mean the owner has no privilege on the table.
		_, err = tx.Exec(ctx, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` NO FORCE ROW LEVEL SECURITY`)
		must(t, err)
		if got := asOwner(table); got != real {
			t.Errorf("%s: with FORCE off the owner sees %d of %d rows, want all: the control is broken", table, got, real)
		}
		_, err = tx.Exec(ctx, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` FORCE ROW LEVEL SECURITY`)
		must(t, err)
	}
	if checked < 10 {
		t.Fatalf("only %d of %d tables had rows to show an effect with", checked, len(tables))
	}

	// Writes too: the owner cannot insert a row for a group nobody put it in.
	_, err = tx.Exec(ctx, `SAVEPOINT w`)
	must(t, err)
	_, err = tx.Exec(ctx, `SET LOCAL ROLE `+role)
	must(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO groups (name, currency, created_by) VALUES ('mine', 'CAD', $1)`, w.x)
	if !rlsRefused(err) {
		t.Errorf("the non-superuser owner inserted into groups: want a row-level security refusal, got %v", err)
	}
	_, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT w`)
	must(t, err)
	_, err = tx.Exec(ctx, `RESET ROLE`)
	must(t, err)
}
