package dbinvariants_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// The jobs table (migration 00006) must refuse every combination of columns that
// the queue's state machine can never produce, so a bug in the claim, heartbeat,
// complete or fail code cannot leave a job that means nothing. Each case is one
// raw INSERT; all run in rolled-back transactions.
//
// The first block must be ACCEPTED: without it a typo in the harness would make
// every refusal below pass for the wrong reason.
func TestJobs_ShapeIsEnforced(t *testing.T) {
	pool := newWorld(t).pool

	type row struct {
		state, attempts, maxAttempts   string
		token, expires, finished, kind string
		payload                        string
	}
	// Columns default to those of a valid queued job; each case overrides some.
	valid := row{
		state: `'queued'`, attempts: `0`, maxAttempts: `3`,
		token: `NULL`, expires: `NULL`, finished: `NULL`, kind: `'k'`, payload: `'{}'`,
	}
	with := func(f func(r *row)) row { r := valid; f(&r); return r }
	running := func(r *row) {
		r.state, r.attempts, r.token, r.expires = `'running'`, `1`, `gen_random_uuid()`, `now() + interval '30 seconds'`
	}
	terminal := func(state string) func(r *row) {
		return func(r *row) { r.state, r.attempts, r.finished = state, `1`, `now()` }
	}

	cases := []struct {
		name string
		row  row
		want string
	}{
		// accepted
		{"queued", valid, noErrExpected},
		{"running with a lease", with(running), noErrExpected},
		{"succeeded with a finish time", with(terminal(`'succeeded'`)), noErrExpected},
		{"dead with a finish time", with(terminal(`'dead'`)), noErrExpected},
		{"attempts equal to max_attempts", with(func(r *row) { r.attempts, r.maxAttempts = `3`, `3` }), noErrExpected},

		// the lease exists exactly while running
		{"running without a token", with(func(r *row) { running(r); r.token = `NULL` }), codeCheck},
		{"running without an expiry", with(func(r *row) { running(r); r.expires = `NULL` }), codeCheck},
		{"queued with a token", with(func(r *row) { r.token = `gen_random_uuid()` }), codeCheck},
		{"queued with an expiry", with(func(r *row) { r.expires = `now()` }), codeCheck},
		{"succeeded still holding a lease", with(func(r *row) { terminal(`'succeeded'`)(r); r.token = `gen_random_uuid()` }), codeCheck},
		{"dead still holding an expiry", with(func(r *row) { terminal(`'dead'`)(r); r.expires = `now()` }), codeCheck},

		// finished_at exists exactly when terminal
		{"succeeded without a finish time", with(func(r *row) { terminal(`'succeeded'`)(r); r.finished = `NULL` }), codeCheck},
		{"dead without a finish time", with(func(r *row) { terminal(`'dead'`)(r); r.finished = `NULL` }), codeCheck},
		{"queued with a finish time", with(func(r *row) { r.finished = `now()` }), codeCheck},
		{"running with a finish time", with(func(r *row) { running(r); r.finished = `now()` }), codeCheck},

		// attempts
		{"more attempts than allowed", with(func(r *row) { r.attempts = `4` }), codeCheck},
		{"negative attempts", with(func(r *row) { r.attempts = `-1` }), codeCheck},
		{"zero max_attempts", with(func(r *row) { r.maxAttempts = `0` }), codeCheck},

		// the rest of the row
		{"unknown state", with(func(r *row) { r.state = `'paused'` }), codeCheck},
		{"empty kind", with(func(r *row) { r.kind = `''` }), codeCheck},
		{"kind over 100 characters", with(func(r *row) { r.kind = `repeat('k', 101)` }), codeCheck},
		{"payload is an array", with(func(r *row) { r.payload = `'[]'` }), codeCheck},
		{"payload is a string", with(func(r *row) { r.payload = `'"x"'` }), codeCheck},
		{"payload is JSON null", with(func(r *row) { r.payload = `'null'` }), codeCheck},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Values are SQL fragments written in this file, never user input.
			sql := `INSERT INTO jobs (kind, payload, state, attempts, max_attempts, lease_token, lease_expires_at, finished_at)
			        VALUES (` + c.row.kind + `, ` + c.row.payload + `, ` + c.row.state + `, ` + c.row.attempts + `, ` +
				c.row.maxAttempts + `, ` + c.row.token + `, ` + c.row.expires + `, ` + c.row.finished + `)`
			err := inTxAlwaysRollback(pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql)
				return err
			})
			wantCode(t, err, c.want, c.name)
		})
	}
}
