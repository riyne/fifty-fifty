package rls_test

import (
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/dbrole"
)

// Tables that are deliberately not under row-level security, with the reason. A table that
// is neither here nor protected fails the test below: whoever adds a table has to decide.
var exempt = map[string]string{
	"jobs":             "the queue: payloads hold ids only, the api inserts and reads back its own job in the same transaction, and the worker owns the rest",
	"goose_db_version": "migration bookkeeping, no privileges for any application role",
}

func (w *world) tables(t *testing.T) (protected, unprotected []string) {
	t.Helper()
	rows, err := w.owner.Query(ctx, `SELECT c.relname, c.relrowsecurity AND c.relforcerowsecurity
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') ORDER BY 1`)
	must(t, err)
	defer rows.Close()
	for rows.Next() {
		var name string
		var forced bool
		must(t, rows.Scan(&name, &forced))
		if forced {
			protected = append(protected, name)
		} else {
			unprotected = append(unprotected, name)
		}
	}
	must(t, rows.Err())
	return
}

// Every table is either enabled and forced, or on the exemption list with a reason.
func TestEveryTableIsProtectedOrDeliberatelyExempt(t *testing.T) {
	w := shared(t)
	prot, unprot := w.tables(t)
	for _, name := range unprot {
		if _, ok := exempt[name]; !ok {
			t.Errorf("table %s has no row-level security and is not on the exemption list: protect it in a migration, or add it to `exempt` with the reason", name)
		}
	}
	for name := range exempt {
		for _, p := range prot {
			if p == name {
				t.Errorf("%s is on the exemption list but is protected: remove it from the list", name)
			}
		}
	}
	// The protected set is the one the visibility tests check.
	want := map[string]bool{}
	for _, s := range protected {
		want[s.name] = true
	}
	for _, p := range prot {
		if !want[p] {
			t.Errorf("table %s is protected but TestEveryTableShowsOnlyTheCallersGroups does not check it: add it to `protected`", p)
		}
		delete(want, p)
	}
	for name := range want {
		t.Errorf("`protected` lists %s, which is not protected in the database", name)
	}
}

type policy struct{ table, name, cmd, qual string }

func (w *world) policiesFor(t *testing.T, role string) []policy {
	t.Helper()
	rows, err := w.owner.Query(ctx, `SELECT tablename, policyname, cmd, COALESCE(qual, '') FROM pg_policies
		WHERE schemaname = 'public' AND $1 = ANY (roles) ORDER BY 1, 2`, role)
	must(t, err)
	defer rows.Close()
	var out []policy
	for rows.Next() {
		var p policy
		must(t, rows.Scan(&p.table, &p.name, &p.cmd, &p.qual))
		out = append(out, p)
	}
	must(t, rows.Err())
	return out
}

// Policies exist for the right roles on the right tables, and none is open to everyone.
func TestPoliciesMatchTheRolesAndGrants(t *testing.T) {
	w := shared(t)
	prot, _ := w.tables(t)

	// Every policy names only the three application roles (never PUBLIC).
	rows, err := w.owner.Query(ctx, `SELECT tablename, policyname, roles::text FROM pg_policies WHERE schemaname = 'public'`)
	must(t, err)
	for rows.Next() {
		var table, name, roles string
		must(t, rows.Scan(&table, &name, &roles))
		for _, r := range strings.Split(strings.Trim(roles, "{}"), ",") {
			if r != dbrole.App && r != dbrole.Worker && r != dbrole.System {
				t.Errorf("policy %s on %s applies to %q", name, table, r)
			}
		}
	}
	must(t, rows.Err())
	rows.Close()

	// fifty_app has a policy on every protected table, and none of them is "true": a
	// wide-open policy would be the way to switch the protection off while leaving it on.
	for _, table := range prot {
		n := 0
		for _, p := range w.policiesFor(t, dbrole.App) {
			if p.table != table {
				continue
			}
			n++
			if p.qual == "true" {
				t.Errorf("fifty_app policy %s on %s is USING (true)", p.name, table)
			}
		}
		if n == 0 {
			t.Errorf("fifty_app has no policy on %s: it would see nothing", table)
		}
	}

	// A role that has any privilege on a protected table has a policy on it (or it sees
	// nothing); and a role with a policy has a privilege (or the policy is dead).
	for _, role := range []string{dbrole.Worker, dbrole.System} {
		have := map[string]bool{}
		for _, p := range w.policiesFor(t, role) {
			have[p.table] = true
		}
		for _, table := range prot {
			var priv bool
			must(t, w.owner.QueryRow(ctx, `SELECT has_table_privilege($1, $2, 'SELECT') OR has_table_privilege($1, $2, 'INSERT')
				OR has_table_privilege($1, $2, 'UPDATE') OR has_table_privilege($1, $2, 'DELETE')`, role, "public."+table).Scan(&priv))
			if priv && !have[table] {
				t.Errorf("%s has privileges on %s but no policy: it would see nothing", role, table)
			}
			if !priv && have[table] {
				t.Errorf("%s has a policy on %s but no privilege: remove the policy", role, table)
			}
		}
	}
}

// The SECURITY DEFINER functions are exactly the two reviewed ones; each pins its search
// path with pg_temp last; EXECUTE is not open to PUBLIC; only the app role may call them.
func TestSecurityDefinerFunctionsAreTheReviewedOnes(t *testing.T) {
	w := shared(t)
	rows, err := w.owner.Query(ctx, `SELECT p.proname, p.oid::regprocedure::text, COALESCE(p.proconfig::text, ''),
			has_function_privilege('public', p.oid, 'EXECUTE'),
			has_function_privilege($1, p.oid, 'EXECUTE'),
			has_function_privilege($2, p.oid, 'EXECUTE'),
			has_function_privilege($3, p.oid, 'EXECUTE')
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND p.prosecdef ORDER BY 1`, dbrole.App, dbrole.Worker, dbrole.System)
	must(t, err)
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name, sig, config string
		var public, app, worker, sys bool
		must(t, rows.Scan(&name, &sig, &config, &public, &app, &worker, &sys))
		seen[name] = true
		if name != "my_group_ids" && name != "redeem_join_code" {
			t.Errorf("SECURITY DEFINER function %s was added: review it (search_path, schema-qualified names, who may call it), then list it here", sig)
		}
		if !strings.Contains(config, "search_path=pg_catalog, public, pg_temp") {
			t.Errorf("%s does not pin its search_path with pg_temp last: %q", sig, config)
		}
		if public || worker || sys || !app {
			t.Errorf("%s EXECUTE: public=%v app=%v worker=%v system=%v, want app only", sig, public, app, worker, sys)
		}
	}
	must(t, rows.Err())
	for _, name := range []string{"my_group_ids", "redeem_join_code"} {
		if !seen[name] {
			t.Errorf("%s is not a SECURITY DEFINER function (or does not exist)", name)
		}
	}
}
