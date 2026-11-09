package rls_test

import (
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/harness/dbrole"
)

// A clean result is only worth something if the checker was shown to catch the fault. On a
// scratch database of its own, this takes the protection off one table at a time, in two
// ways (row-level security switched off; the fifty_app policy replaced by USING (true)), and
// requires the same check that passes elsewhere to fail, and to name the table. A policy
// that is missing, or too wide, cannot slip through the visibility tests unnoticed.
func TestTheVisibilityCheckNoticesAnUnprotectedTable(t *testing.T) {
	w := newWorld(t)
	t.Cleanup(w.close)

	if m := w.mismatches(t, w.app, w.A.m1); len(m) != 0 {
		t.Fatalf("the check fails on an untouched database: %v", m)
	}

	mustFail := func(t *testing.T, what, table string) {
		t.Helper()
		m := w.mismatches(t, w.app, w.A.m1)
		found := false
		for _, line := range m {
			if strings.HasPrefix(line, table+":") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the check did not notice (mismatches: %v)", what, m)
		}
	}

	for _, s := range protected {
		t.Run("disable "+s.name, func(t *testing.T) {
			w.exec(t, `ALTER TABLE `+s.name+` DISABLE ROW LEVEL SECURITY`)
			defer w.exec(t, `ALTER TABLE `+s.name+` ENABLE ROW LEVEL SECURITY`)
			mustFail(t, "row-level security disabled on "+s.name, s.name)
		})
	}

	// Every fifty_app policy that decides what is read, replaced by USING (true).
	for _, p := range w.policiesFor(t, dbrole.App) {
		if p.cmd != "ALL" && p.cmd != "SELECT" {
			continue
		}
		t.Run("widen "+p.table+"."+p.name, func(t *testing.T) {
			w.exec(t, `ALTER POLICY `+p.name+` ON `+p.table+` USING (true)`)
			defer w.exec(t, `ALTER POLICY `+p.name+` ON `+p.table+` USING (`+p.qual+`)`)
			mustFail(t, "policy "+p.name+" on "+p.table+" widened", p.table)
		})
	}

	// And the write side: the one insert policy that matters most. With its check widened,
	// a raw INSERT adds the user to another group, which is what the write test forbids.
	t.Run("widen the memberships insert check", func(t *testing.T) {
		w.exec(t, `ALTER POLICY app_insert_owner ON memberships WITH CHECK (true)`)
		defer w.exec(t, `ALTER POLICY app_insert_owner ON memberships WITH CHECK (user_id = app_user_id() AND role = 'owner'
		                 AND EXISTS (SELECT 1 FROM groups g WHERE g.id = group_id AND g.created_by = app_user_id()))`)
		tx := w.as(t, w.A.m1)
		_, err := tx.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'member')`, w.B.id, w.A.m1)
		// Release the row lock before the deferred ALTER POLICY, which needs an exclusive one.
		_ = tx.Rollback(ctx)
		if rlsRefused(err) {
			t.Fatal("the insert is still refused: the fault did not take effect, so the write test proves nothing")
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// After all of that the original check passes again: the faults were undone.
	if m := w.mismatches(t, w.app, w.A.m1); len(m) != 0 {
		t.Fatalf("the database was not restored: %v", m)
	}
}
