package dbinvariants_test

import (
	"context"
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/harness/dbrole"
	"github.com/riyne/fifty-fifty/internal/harness/testutil"
)

// What each database role may do to each table, spelled out. A table that is not in
// the maps below fails the test, so a new table has to be classified here by whoever
// adds it, and a grant that someone widens later shows as a diff against this list.
// Letters: S select, I insert, U update, D delete. Absent means no privilege at all.
//
// fifty_app is the user-request path (row-level security limits it); the
// ledger tables and audit_events have no UPDATE or DELETE for it (migration 00004,
// 00005). fifty_worker and fifty_system are migration 00015 and were derived from the
// statements each process runs (the real-process tests in test/e2e run them as these
// roles).
var expectedPrivileges = map[string]map[string]string{
	dbrole.App: {
		"audit_events": "SI", "claims": "SID", "expense_items": "SIUD", "expenses": "SIU",
		"groups": "SIU", "idempotency_keys": "SIU", "jobs": "SI", "join_codes": "SIU",
		"ledger_entries": "SI", "ledger_transactions": "SI", "memberships": "SI",
		"receipt_quota": "SIU", "receipts": "SI", "settlements": "SI", "users": "S",
	},
	dbrole.Worker: {
		"audit_events": "I", "expense_items": "SI", "expenses": "SU", "groups": "S",
		"idempotency_keys": "SD", "jobs": "SIUD", "ledger_entries": "SI",
		"ledger_transactions": "SI", "receipts": "SU", "settlements": "SU",
	},
	dbrole.System: {
		"audit_events": "I", "groups": "S", "ledger_entries": "SI", "ledger_transactions": "SI",
		"settlements": "SU", "users": "SI",
	},
}

// Tables nobody but the migration owner touches.
var ownerOnlyTables = map[string]bool{"goose_db_version": true}

func TestRoles_PrivilegesAreExactlyTheListedOnes(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) < 15 {
		t.Fatalf("only %d tables found: %v", len(tables), tables)
	}

	for _, table := range tables {
		if !ownerOnlyTables[table] && !classifiedForSomeRole(table) {
			t.Errorf("table %s is not classified in expectedPrivileges: say which roles may touch it", table)
		}
	}
	for role, want := range expectedPrivileges {
		for _, table := range tables {
			var got string
			err := pool.QueryRow(ctx, `SELECT concat_ws('',
				CASE WHEN has_table_privilege($1, $2, 'SELECT') THEN 'S' END,
				CASE WHEN has_table_privilege($1, $2, 'INSERT') THEN 'I' END,
				CASE WHEN has_table_privilege($1, $2, 'UPDATE') THEN 'U' END,
				CASE WHEN has_table_privilege($1, $2, 'DELETE') THEN 'D' END)`, role, "public."+table).Scan(&got)
			if err != nil {
				t.Fatal(err)
			}
			if got != want[table] {
				t.Errorf("%s on %s: has %q, want %q", role, table, got, want[table])
			}
		}
		for table := range want {
			if !contains(tables, table) {
				t.Errorf("%s lists %s, which does not exist", role, table)
			}
		}
	}
}

func classifiedForSomeRole(table string) bool {
	for _, m := range expectedPrivileges {
		if _, ok := m[table]; ok {
			return true
		}
	}
	return false
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// fifty_system updates users only to make INSERT ... ON CONFLICT DO UPDATE return the
// existing row, which needs UPDATE on the one column it sets. Nothing else about a user
// (the email, the display name) may be changed by it, and fifty_worker cannot see users.
func TestRoles_SystemMayUpdateOnlyTheUsersIssuerColumn(t *testing.T) {
	pool := testutil.Pool(t)
	for _, tc := range []struct {
		role, col string
		want      bool
	}{
		{dbrole.System, "issuer", true},
		{dbrole.System, "subject", false},
		{dbrole.System, "email", false},
		{dbrole.System, "display_name", false},
		{dbrole.System, "id", false},
		{dbrole.Worker, "issuer", false},
	} {
		var got bool
		if err := pool.QueryRow(context.Background(), `SELECT has_column_privilege($1, 'public.users', $2, 'UPDATE')`, tc.role, tc.col).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s UPDATE on users.%s = %v, want %v", tc.role, tc.col, got, tc.want)
		}
	}
}

// None of the three roles is more than a plain login: not a superuser, no BYPASSRLS
// (row-level security applies to them or is waived by an explicit policy, never by an
// attribute), cannot create databases or roles, owns nothing, and is a member of no
// other role (which would hand it that role's privileges).
func TestRoles_AreOnlyPlainLogins(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	for _, role := range []string{dbrole.App, dbrole.Worker, dbrole.System} {
		var super, bypass, createdb, createrole, replication, login, inherit bool
		err := pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls, rolcreatedb, rolcreaterole, rolreplication, rolcanlogin, rolinherit
			FROM pg_roles WHERE rolname = $1`, role).Scan(&super, &bypass, &createdb, &createrole, &replication, &login, &inherit)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if super || bypass || createdb || createrole || replication || inherit || !login {
			t.Errorf("%s: super=%v bypassrls=%v createdb=%v createrole=%v replication=%v inherit=%v login=%v", role, super, bypass, createdb, createrole, replication, inherit, login)
		}

		var memberOf []string
		mrows, err := pool.Query(ctx, `SELECT g.rolname FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid
			JOIN pg_roles r ON r.oid = m.member WHERE r.rolname = $1`, role)
		if err != nil {
			t.Fatal(err)
		}
		for mrows.Next() {
			var g string
			if err := mrows.Scan(&g); err != nil {
				t.Fatal(err)
			}
			memberOf = append(memberOf, g)
		}
		mrows.Close()
		if len(memberOf) > 0 {
			t.Errorf("%s is a member of %s", role, strings.Join(memberOf, ", "))
		}

		var canCreate, ownsAny bool
		if err := pool.QueryRow(ctx, `SELECT has_schema_privilege($1, 'public', 'CREATE')`, role).Scan(&canCreate); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner WHERE r.rolname = $1)`, role).Scan(&ownsAny); err != nil {
			t.Fatal(err)
		}
		if canCreate || ownsAny {
			t.Errorf("%s: can create in public = %v, owns relations = %v", role, canCreate, ownsAny)
		}
	}
}

// ---- what migration 00017 took from fifty_app, tried for real ----
//
// The matrix above says which privileges exist. These show the refusals happen, for the
// statements a bug or an injected statement in the user-request path would try. The world's
// connections carry the identity of a member of every group, so row-level security would
// let each of these through: it is the missing privilege that refuses them.
func TestRoles_AppIsRefusedTheCommandsItNeverRuns(t *testing.T) {
	w := newWorld(t)
	for _, tc := range []struct{ name, sql string }{
		{"delete a group", `DELETE FROM groups WHERE id = $1`},
		{"delete an expense", `DELETE FROM expenses WHERE group_id = $1`},
		{"rewrite a settlement", `UPDATE settlements SET amount_minor = 1 WHERE group_id = $1`},
		{"delete a settlement", `DELETE FROM settlements WHERE group_id = $1`},
		{"change a membership's role", `UPDATE memberships SET role = 'owner' WHERE group_id = $1`},
		{"remove a member", `DELETE FROM memberships WHERE group_id = $1`},
		{"mark a receipt parsed", `UPDATE receipts SET parse_outcome = 'claiming' WHERE group_id = $1`},
		{"delete a receipt", `DELETE FROM receipts WHERE group_id = $1`},
		{"delete a join code", `DELETE FROM join_codes WHERE group_id = $1`},
		{"change a claim in place", `UPDATE claims SET member_id = member_id WHERE group_id = $1`},
	} {
		_, err := w.pool.Exec(ctx, tc.sql, w.g)
		wantCode(t, err, codePrivilege, "fifty_app tried to "+tc.name)
	}
	// Not group-scoped.
	for name, sql := range map[string]string{
		"delete a job":                 `DELETE FROM jobs WHERE false`,
		"claim or fail a job":          `UPDATE jobs SET state = state WHERE false`,
		"create a user":                `INSERT INTO users (issuer, subject) VALUES ('x', 'y')`,
		"change a user":                `UPDATE users SET display_name = 'z' WHERE false`,
		"delete an idempotency key":    `DELETE FROM idempotency_keys WHERE false`,
		"delete a receipt quota count": `DELETE FROM receipt_quota WHERE false`,
	} {
		_, err := w.pool.Exec(ctx, sql)
		wantCode(t, err, codePrivilege, "fifty_app tried to "+name)
	}
	// What it does need still works, including the two kinds of UPDATE a verb search misses:
	// a row lock (needs UPDATE on the table) and ON CONFLICT DO UPDATE.
	for _, ok := range []string{
		`SELECT 1 FROM groups WHERE id = $1 FOR NO KEY UPDATE`,
		`SELECT 1 FROM expenses WHERE group_id = $1 LIMIT 1 FOR SHARE`,
	} {
		if _, err := w.pool.Exec(ctx, ok, w.g); err != nil {
			t.Errorf("fifty_app lost a privilege its row locks need: %s: %v", ok, err)
		}
	}
	_, err := w.pool.Exec(ctx, `INSERT INTO receipt_quota (user_id, day, used) VALUES ($1, current_date, 1)
		ON CONFLICT (user_id, day) DO UPDATE SET used = receipt_quota.used + 1`, w.actor)
	if err != nil {
		t.Errorf("fifty_app lost the privilege ON CONFLICT DO UPDATE needs: %v", err)
	}
}

// A table added by a later migration is not handed to fifty_app automatically (it used to
// be: 00004). Shown by creating one as the owner in a transaction that is rolled back.
func TestRoles_NewTablesAreNotGrantedToTheAppRoleAutomatically(t *testing.T) {
	w := newWorld(t)
	tx, err := w.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // always rolled back
	if _, err := tx.Exec(ctx, `CREATE TABLE zz_new_table_probe (id serial PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{dbrole.App, dbrole.Worker, dbrole.System} {
		var any bool
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege($1, 'zz_new_table_probe', 'SELECT, INSERT, UPDATE, DELETE')
			OR has_sequence_privilege($1, 'zz_new_table_probe_id_seq', 'USAGE, SELECT')`, role).Scan(&any); err != nil {
			t.Fatal(err)
		}
		if any {
			t.Errorf("%s has a privilege on a table nobody granted it: a later migration must say what each role may do", role)
		}
	}
}

// Every role could create temporary tables (PUBLIC has TEMPORARY on a new database). Nothing
// in the product does; the privilege is gone from PUBLIC (00017).
func TestRoles_NoOneCanCreateTemporaryTables(t *testing.T) {
	w := newWorld(t)
	for _, role := range []string{dbrole.App, dbrole.Worker, dbrole.System} {
		var can bool
		if err := w.owner.QueryRow(ctx, `SELECT has_database_privilege($1, current_database(), 'TEMPORARY')`, role).Scan(&can); err != nil {
			t.Fatal(err)
		}
		if can {
			t.Errorf("%s may create temporary tables", role)
		}
	}
	_, err := w.pool.Exec(ctx, `CREATE TEMP TABLE zz_probe (x int)`)
	wantCode(t, err, codePrivilege, "fifty_app creating a temporary table")
}
