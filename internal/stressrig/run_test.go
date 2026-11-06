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
		"edit/200", "race_edit_conflict", "race_edit_rounds",
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
		"finalize/200", "void/200", "race_rounds", "race_double_finalize_refused",
		"edit/200", "race_edit_conflict", "race_edit_rounds",
		"settle/202", "refused/settle", "replayed/settle", "settlements_succeeded"} {
		full.Counts[k] = 1
	}
	full.Counts["race_mixed"] = RaceMixedFloor
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

// The race_mixed floor and the edit race are required only where the protection they
// exercise is on: with the claim lock off nothing is refused around a finalize, with the
// finalize lock off the finalize does not wait for claims, and with the edit lock or the
// version check off two edits from one version are not refused. A floor that applied to
// those runs flagged two recorded claim_lock runs as vacuous (DESIGN section 13).
func TestVacuous_RaceFloorsFollowTheControls(t *testing.T) {
	base := func() Result {
		r := Result{Locks: LockWaits{SamplesWaiting: 1}, Counts: map[string]int64{}}
		for _, k := range []string{"replayed/create", "duplicate_waited_and_replayed/create", "abandoned/create", "claim/409_not_claimable",
			"finalize/200", "void/200", "race_rounds", "race_double_finalize_refused", "edit/200", "race_edit_rounds",
			"race_edit_conflict", "race_mixed"} {
			r.Counts[k] = 1000
		}
		r.Counts["race_mixed"] = RaceMixedFloor - 1
		return r
	}
	p := Params{RaceLoops: 1}
	if v := base().Vacuous(p); len(v) != 1 || !strings.Contains(v[0], "race_mixed") {
		t.Fatalf("a race_mixed below the floor with both locks on must be flagged: %v", v)
	}
	r := base()
	r.Counts["race_mixed"] = RaceMixedFloor
	if v := r.Vacuous(p); len(v) != 0 {
		t.Fatalf("at the floor is enough: %v", v)
	}
	for _, off := range []string{"claim_lock", "finalize_lock"} {
		q := p
		q.Controls = []string{off}
		r := base()
		r.Counts["race_mixed"] = 0
		if v := r.Vacuous(q); len(v) != 0 {
			t.Errorf("with %s off a low race_mixed is by construction and must not be flagged: %v", off, v)
		}
	}
	for _, off := range []string{"edit_lock", "edit_version_check"} {
		q := p
		q.Controls = []string{off}
		r := base()
		r.Counts["race_mixed"] = RaceMixedFloor
		r.Counts["race_edit_conflict"] = 0
		if v := r.Vacuous(q); len(v) != 0 {
			t.Errorf("with %s off a missing edit conflict must not be flagged: %v", off, v)
		}
	}
	r = base()
	r.Counts["race_mixed"] = RaceMixedFloor
	r.Counts["race_edit_conflict"] = 0
	if v := r.Vacuous(p); len(v) != 1 || !strings.Contains(v[0], "race_edit_conflict") {
		t.Errorf("with the edit protections on, no edit conflict must be flagged: %v", v)
	}
}

func TestWorkerLogErrors(t *testing.T) {
	log := strings.Join([]string{
		`{"time":"2026-11-01T00:00:00Z","level":"INFO","msg":"worker started"}`,
		`{"time":"2026-11-01T00:00:01Z","level":"ERROR","msg":"job failed","err":"post: Post \"http://127.0.0.1:50234/v1/payments\": context deadline exceeded after 10.0s"}`,
		`{"time":"2026-11-01T00:00:02Z","level":"ERROR","msg":"job failed","err":"post: Post \"http://127.0.0.1:50999/v1/payments\": context deadline exceeded after 10.0s"}`,
		`{"time":"2026-11-01T00:00:03Z","level":"WARN","msg":"job failed; will retry","err":"ERROR: duplicate key value violates unique constraint \"ledger_transactions_one_settlement_posting\" (SQLSTATE 23505)"}`,
		`2026/11/01 00:00:04 not json at all`,
	}, "\n")
	got := workerLogErrors(log)
	if len(got) != 2 {
		t.Fatalf("classes = %v", got)
	}
	if got["WARN unique:ledger_transactions_one_settlement_posting"] != 1 {
		t.Errorf("constraint class missing: %v", got)
	}
	n := 0
	for k, v := range got {
		if strings.HasPrefix(k, "ERROR client_gone") { // a deadline in the message is a client going away
			n += v
		}
	}
	if n != 2 {
		t.Errorf("the two timeouts should count together: %v", got)
	}
}
