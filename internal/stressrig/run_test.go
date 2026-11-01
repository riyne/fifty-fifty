package stressrig

import (
	"strings"
	"testing"
)

func TestClassifyLogError(t *testing.T) {
	cases := []struct{ msg, err, want string }{
		{"finalize", "lock expense: context canceled", "client_gone"},
		{"create", "claim key: context deadline exceeded", "client_gone"},
		{"void", "begin: write failed: write tcp 127.0.0.1:1->127.0.0.1:2: i/o timeout", "client_gone_io_timeout"},
		{"finalize", `ERROR: duplicate key value violates unique constraint "ledger_transactions_one_expense_posting" (SQLSTATE 23505)`, "unique:ledger_transactions_one_expense_posting"},
		{"add claim", `ERROR: insert or update on table "claims" violates foreign key constraint "claims_fk" (SQLSTATE 23503)`, "foreign key:claims_fk"},
		{"add claim", "ERROR: deadlock detected (SQLSTATE 40P01)", "other:add claim: ERROR: deadlock detected (SQLSTATE 40P01)"},
		{"finalize", "allocate: item 694e7646-4ca1-4cc2-9e4d-8a5ea68d3536: allocator: item has no claimants", "other:finalize: allocate: item <id>: allocator: item has no claimants"},
	}
	for _, c := range cases {
		if got := classifyLogError(c.msg, c.err); got != c.want {
			t.Errorf("classify(%q, %q) = %q, want %q", c.msg, c.err, got, c.want)
		}
	}
}

func TestLogicalRows(t *testing.T) {
	cases := []struct {
		l    logical
		want Rows
	}{
		{logical{succ: 1}, One},
		{logical{succ: 2, unknown: 1}, One},
		{logical{refused: 2}, Zero},
		{logical{refused: 1, unknown: 1}, ZeroOrOne}, // an attempt we never saw an answer for may have been carried out
		{logical{unknown: 3}, ZeroOrOne},
		{logical{}, ZeroOrOne},
	}
	for _, c := range cases {
		if got := c.l.rows(); got != c.want {
			t.Errorf("%+v: rows = %v, want %v", c.l, got, c.want)
		}
	}
}

func TestVacuousAndClean(t *testing.T) {
	p := Params{RaceLoops: 1, SettleWorkers: 1}
	empty := Result{Counts: map[string]int64{}}
	v := empty.Vacuous(p)
	for _, want := range []string{"replayed/create", "duplicate_waited_and_replayed/create", "abandoned/create", "claim/409_not_claimable",
		"finalize/200", "void/200", "race_rounds", "race_mixed", "race_double_finalize_refused", "backend waiting for a lock",
		"settle/202", "refused/settle", "replayed/settle", "reached succeeded"} {
		if !strings.Contains(strings.Join(v, "\n"), want) {
			t.Errorf("an empty run must be vacuous about %q; got %v", want, v)
		}
	}
	if empty.Clean(p) {
		t.Error("a run that did nothing must not be clean")
	}

	full := Result{Locks: LockWaits{SamplesWaiting: 1}, Counts: map[string]int64{}}
	for _, k := range []string{"replayed/create", "duplicate_waited_and_replayed/create", "abandoned/create", "claim/409_not_claimable",
		"finalize/200", "void/200", "race_rounds", "race_mixed", "race_double_finalize_refused",
		"settle/202", "refused/settle", "replayed/settle", "settlements_succeeded"} {
		full.Counts[k] = 1
	}
	if v := full.Vacuous(p); len(v) != 0 {
		t.Fatalf("a run with everything must not be vacuous: %v", v)
	}
	if !full.Clean(p) {
		t.Fatal("a run with every situation, no violation and no errors is clean")
	}
	// Each way to fail.
	bad := full
	bad.Counts = map[string]int64{}
	for k, v := range full.Counts {
		bad.Counts[k] = v
	}
	bad.Counts["http_5xx"] = 1
	if bad.Clean(p) {
		t.Error("a 5xx must make a run unclean")
	}
	bad.Counts = map[string]int64{LogErrorPrefix + "unique:x": 1}
	for k, v := range full.Counts {
		bad.Counts[k] = v
	}
	if bad.Clean(p) || len(bad.ServerErrors()) != 1 {
		t.Errorf("a logged constraint violation must make a run unclean: %v", bad.ServerErrors())
	}
	bad.Counts = map[string]int64{LogErrorPrefix + "client_gone": 5, LogErrorPrefix + "client_gone_io_timeout": 2}
	for k, v := range full.Counts {
		bad.Counts[k] = v
	}
	if !bad.Clean(p) {
		t.Error("abandoned requests are expected and must not make a run unclean")
	}
	bad.Unresolved = 1
	if bad.Clean(p) {
		t.Error("an unresolved settlement must make a run unclean")
	}
	bad = full
	bad.Unexpected = []string{"x"}
	if bad.Clean(p) {
		t.Error("an unexpected response must make a run unclean")
	}
	bad = full
	bad.Report.Counts = map[Invariant]int{DuplicateKey: 1}
	if bad.Clean(p) {
		t.Error("a violated invariant must make a run unclean")
	}
}
