// Package rlsbench measures what row-level security (migration 00016) and the identity
// seam (internal/dbscope) cost, at the database level, with the queries the handlers use.
//
// Three variants run on the same seeded database, in a closed loop of concurrent workers
// that each pick a random user and a random operation:
//
//	baseline       row-level security DISABLED, queries on the plain pool (what the api did
//	               before row-level security: autocommit reads, a transaction for writes)
//	identity only  row-level security DISABLED, queries through dbscope (a read is BEGIN,
//	               set_config, the statement, COMMIT; a write is a transaction that starts
//	               with set_config)
//	policies on    row-level security ENABLED and FORCED on the 14 tables, through dbscope
//
// identity only against baseline is the cost of the seam; policies on against identity
// only is the cost of evaluating the policies. The variants are switched with ALTER TABLE
// run by the owner while the workers are idle, so no runtime flag exists in the product.
// Every variant must give the same answers to the same questions (a fixed list is asked
// first and the row counts compared), and each variant reads back from the catalog how
// many tables have row-level security on and what an unguarded count sees, so a toggle
// that did not take effect cannot be reported as a measurement.
//
// This is one machine, Postgres in Docker on the same host, no network between the
// workers and the database, synthetic data and a mix of operations chosen by the author.
// It is a measurement of this code on this setup, not a general statement about RLS.
package rlsbench

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/controls"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/dbscope"
	"github.com/riyne/fifty-fifty/internal/harness/settlerig"
)

// Params are the knobs of a run.
type Params struct {
	OwnerURL, AppURL string
	Groups           int // groups; each has MembersPerGroup members
	MembersPerGroup  int
	ExpensesPerGroup int // half finalized (with a posting), half claiming
	Workers          int // concurrent closed-loop workers (and connections)
	Warmup, Measure  time.Duration
	Reps             int // repetitions, each on a fresh database, with the variants in a rotating order
	Seed             uint64
	Logf             func(format string, args ...any)
}

// Variant names, in the order baseline -> identity only -> policies on.
const (
	Baseline     = "baseline"
	IdentityOnly = "identity_only"
	PoliciesOn   = "policies_on"
)

// The operations, with their weights in the mix (percent).
var ops = []struct {
	name   string
	weight int
}{
	{"list_groups", 10},
	{"list_expenses", 25},
	{"get_expense", 30},
	{"balances", 20},
	{"claim_unclaim", 15},
}

// Stat is the latency summary of one operation in one variant of one repetition.
type Stat struct {
	N        int     `json:"n"`
	P50Ms    float64 `json:"p50_ms"`
	P99Ms    float64 `json:"p99_ms"`
	MeanMs   float64 `json:"mean_ms"`
	OpsPerS  float64 `json:"ops_per_s"`
	Failures int     `json:"failures"`
}

// VariantResult is one variant in one repetition.
type VariantResult struct {
	Variant          string          `json:"variant"`
	Total            Stat            `json:"total"`
	ByOp             map[string]Stat `json:"by_op"`
	TablesWithRLS    int             `json:"tables_with_rls_enabled_and_forced"`
	UnguardedCount   int             `json:"unguarded_expense_count_as_a_user"`
	CheckSignature   []int           `json:"-"`
	ElapsedMeasureMs float64         `json:"measure_ms"`
}

// Rep is one repetition: a fresh database, the three variants in Order.
type Rep struct {
	Rep            int                      `json:"rep"`
	Order          []string                 `json:"order"`
	Seed           map[string]int           `json:"seeded_rows"`
	Variants       map[string]VariantResult `json:"variants"`
	OwnerExpenses  int                      `json:"expenses_in_the_table"`
	Plans          map[string]string        `json:"plans,omitempty"`
	SeedMs, RunMs  float64                  `json:"-"`
	ChecksCompared int                      `json:"answers_compared_across_variants"`
}

var orders = [][]string{
	{Baseline, IdentityOnly, PoliciesOn}, {IdentityOnly, PoliciesOn, Baseline}, {PoliciesOn, Baseline, IdentityOnly},
	{PoliciesOn, IdentityOnly, Baseline}, {Baseline, PoliciesOn, IdentityOnly}, {IdentityOnly, Baseline, PoliciesOn},
}

// protectedTables are the tables the migration put row-level security on.
var protectedTables = []string{"users", "groups", "memberships", "join_codes", "idempotency_keys", "audit_events", "expenses",
	"expense_items", "claims", "ledger_transactions", "ledger_entries", "settlements", "receipts", "receipt_quota"}

// Run does Reps repetitions and returns them in order.
func Run(ctx context.Context, p Params) ([]Rep, error) {
	if p.Logf == nil {
		p.Logf = func(string, ...any) {}
	}
	var out []Rep
	for i := 0; i < p.Reps; i++ {
		order := orders[i%len(orders)]
		rep, err := runRep(ctx, p, i+1, order)
		if err != nil {
			return out, fmt.Errorf("repetition %d: %w", i+1, err)
		}
		out = append(out, rep)
	}
	return out, nil
}

type world struct {
	owner, app *pgxpool.Pool
	// per user: the groups; per group: members, expenses
	userGroups map[uuid.UUID][]uuid.UUID
	users      []uuid.UUID
	groupFin   map[uuid.UUID][]uuid.UUID // finalized expenses
	groupClm   map[uuid.UUID][]claimable // claiming expenses with an item nobody claims
	groupExp   map[uuid.UUID][]uuid.UUID // all expenses
}

type claimable struct{ expense, item uuid.UUID }

func runRep(ctx context.Context, p Params, n int, order []string) (Rep, error) {
	rep := Rep{Rep: n, Order: order, Variants: map[string]VariantResult{}}
	sc, err := settlerig.NewScratch(ctx, p.OwnerURL, p.AppURL)
	if err != nil {
		return rep, err
	}
	defer func() { _ = sc.Drop(context.Background()) }()

	cfg := sc.Pool.Config().Copy()
	cfg.MaxConns = int32(p.Workers)
	cfg.MinConns = int32(p.Workers) // connect up front, so no variant pays for opening connections
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return rep, err
	}
	defer app.Close()

	t0 := time.Now()
	w := &world{owner: sc.Owner, app: app}
	if rep.Seed, err = seed(ctx, sc.Owner, p); err != nil {
		return rep, fmt.Errorf("seed: %w", err)
	}
	if err := w.load(ctx); err != nil {
		return rep, err
	}
	rep.SeedMs = float64(time.Since(t0).Milliseconds())
	if err := sc.Owner.QueryRow(ctx, `SELECT count(*) FROM expenses`).Scan(&rep.OwnerExpenses); err != nil {
		return rep, err
	}
	p.Logf("repetition %d: seeded %v in %.1fs; order %v", n, rep.Seed, rep.SeedMs/1000, order)

	var reference []int
	for _, v := range order {
		if err := w.setVariant(ctx, v); err != nil {
			return rep, err
		}
		res, err := w.measure(ctx, p, v, n)
		if err != nil {
			return rep, fmt.Errorf("variant %s: %w", v, err)
		}
		// The same questions must have the same answers in every variant.
		if reference == nil {
			reference = res.CheckSignature
		} else if !equalInts(reference, res.CheckSignature) {
			return rep, fmt.Errorf("variant %s answered the fixed questions differently from the first variant run: row-level security must not change what a legitimate query returns", v)
		}
		rep.ChecksCompared = len(reference)
		rep.Variants[v] = res
		p.Logf("repetition %d, %-13s %8.0f ops/s  p50 %.2f ms  p99 %.2f ms  (RLS on %d tables; unguarded count %d)", n, v,
			res.Total.OpsPerS, res.Total.P50Ms, res.Total.P99Ms, res.TablesWithRLS, res.UnguardedCount)
	}
	if n == 1 {
		if err := w.setVariant(ctx, PoliciesOn); err != nil {
			return rep, err
		}
		rep.Plans, err = w.plans(ctx)
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// seed fills the database in one owner transaction with bulk SQL and returns row counts.
func seed(ctx context.Context, owner *pgxpool.Pool, p Params) (map[string]int, error) {
	conn, err := owner.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	M := p.MembersPerGroup
	steps := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (issuer, subject) SELECT 'bench', 'u' || g FROM generate_series(1, $1::int) g`, []any{p.Groups * M}},
		{`CREATE TEMP TABLE bu ON COMMIT DROP AS SELECT id, row_number() OVER (ORDER BY id) AS n FROM users`, nil},
		{`INSERT INTO groups (name, currency, created_by)
		  SELECT 'g' || gi, 'CAD', (SELECT id FROM bu WHERE n = (gi - 1) * $2::int + 1) FROM generate_series(1, $1::int) gi`, []any{p.Groups, M}},
		{`CREATE TEMP TABLE bg ON COMMIT DROP AS SELECT id, substring(name FROM 2)::int AS gi FROM groups`, nil},
		{`INSERT INTO memberships (group_id, user_id, role)
		  SELECT bg.id, bu.id, CASE WHEN (bu.n - 1) % $1::int = 0 THEN 'owner' ELSE 'member' END
		  FROM bu JOIN bg ON bg.gi = ((bu.n - 1) / $1::int) + 1`, []any{M}},
		// every fifth user is also in the next group, so users belong to one or two groups
		{`INSERT INTO memberships (group_id, user_id, role)
		  SELECT bg.id, bu.id, 'member' FROM bu JOIN bg ON bg.gi = ((((bu.n - 1) / $1::int) + 1) % $2::int) + 1 WHERE bu.n % 5 = 0`, []any{M, p.Groups}},
		{`INSERT INTO expenses (group_id, payer_id, description, status, finalized_at)
		  SELECT bg.id, (SELECT user_id FROM memberships WHERE group_id = bg.id AND role = 'owner'), 'dinner ' || e,
		         CASE WHEN e % 2 = 0 THEN 'finalized' ELSE 'claiming' END,
		         CASE WHEN e % 2 = 0 THEN now() END
		  FROM bg CROSS JOIN generate_series(1, $1::int) e`, []any{p.ExpensesPerGroup}},
		{`INSERT INTO expense_items (expense_id, position, description, price_minor)
		  SELECT e.id, pos, 'item ' || pos, 100 * (pos + 1) FROM expenses e CROSS JOIN generate_series(0, 4) pos`, nil},
		// claiming expenses: items 0-2 claimed by the owner, 0-1 also by the first member; items 3 and 4 unclaimed
		{`INSERT INTO claims (item_id, expense_id, group_id, member_id)
		  SELECT i.id, i.expense_id, e.group_id, o.user_id FROM expense_items i
		  JOIN expenses e ON e.id = i.expense_id AND e.status = 'claiming'
		  JOIN memberships o ON o.group_id = e.group_id AND o.role = 'owner' WHERE i.position < 3`, nil},
		{`INSERT INTO claims (item_id, expense_id, group_id, member_id)
		  SELECT i.id, i.expense_id, e.group_id, (SELECT user_id FROM memberships WHERE group_id = e.group_id AND role = 'member' ORDER BY user_id LIMIT 1)
		  FROM expense_items i JOIN expenses e ON e.id = i.expense_id AND e.status = 'claiming' WHERE i.position < 2`, nil},
		// finalized expenses: one posting each, two balanced entries
		{`WITH tx AS (
		    INSERT INTO ledger_transactions (group_id, kind, expense_id, created_by)
		    SELECT group_id, 'expense', id, payer_id FROM expenses WHERE status = 'finalized' RETURNING id, group_id)
		  INSERT INTO ledger_entries (transaction_id, group_id, member_id, amount_minor)
		  SELECT tx.id, tx.group_id, m.user_id, CASE WHEN m.role = 'owner' THEN 500 ELSE -500 END
		  FROM tx JOIN memberships m ON m.group_id = tx.group_id
		   AND (m.role = 'owner' OR m.user_id = (SELECT user_id FROM memberships WHERE group_id = tx.group_id AND role = 'member' ORDER BY user_id LIMIT 1))`, nil},
	}
	for i, s := range steps {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return nil, fmt.Errorf("seed step %d: %w", i+1, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// Planner statistics, as a database that had been in use would have.
	if _, err := owner.Exec(ctx, `ANALYZE`); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, t := range []string{"users", "groups", "memberships", "expenses", "expense_items", "claims", "ledger_transactions", "ledger_entries"} {
		var n int
		if err := owner.QueryRow(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil {
			return nil, err
		}
		counts[t] = n
	}
	return counts, nil
}

// load reads, as the owner, what the workers need to pick valid operations.
func (w *world) load(ctx context.Context) error {
	w.userGroups = map[uuid.UUID][]uuid.UUID{}
	rows, err := w.owner.Query(ctx, `SELECT user_id, group_id FROM memberships ORDER BY user_id, group_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var u, g uuid.UUID
		if err := rows.Scan(&u, &g); err != nil {
			return err
		}
		if _, ok := w.userGroups[u]; !ok {
			w.users = append(w.users, u)
		}
		w.userGroups[u] = append(w.userGroups[u], g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	w.groupFin, w.groupClm, w.groupExp = map[uuid.UUID][]uuid.UUID{}, map[uuid.UUID][]claimable{}, map[uuid.UUID][]uuid.UUID{}
	rows, err = w.owner.Query(ctx, `SELECT e.group_id, e.id, e.status,
	    (SELECT i.id FROM expense_items i WHERE i.expense_id = e.id AND i.position = 4) FROM expenses e ORDER BY e.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g, e uuid.UUID
		var status string
		var item *uuid.UUID
		if err := rows.Scan(&g, &e, &status, &item); err != nil {
			return err
		}
		w.groupExp[g] = append(w.groupExp[g], e)
		if status == "finalized" {
			w.groupFin[g] = append(w.groupFin[g], e)
		} else if item != nil {
			w.groupClm[g] = append(w.groupClm[g], claimable{e, *item})
		}
	}
	return rows.Err()
}

// setVariant switches row-level security with ALTER TABLE, run by the owner while idle.
func (w *world) setVariant(ctx context.Context, v string) error {
	for _, t := range protectedTables {
		var stmts []string
		if v == PoliciesOn {
			stmts = []string{`ALTER TABLE ` + t + ` ENABLE ROW LEVEL SECURITY`, `ALTER TABLE ` + t + ` FORCE ROW LEVEL SECURITY`}
		} else {
			stmts = []string{`ALTER TABLE ` + t + ` NO FORCE ROW LEVEL SECURITY`, `ALTER TABLE ` + t + ` DISABLE ROW LEVEL SECURITY`}
		}
		for _, s := range stmts {
			if _, err := w.owner.Exec(ctx, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// access is how a variant reaches the database for a user.
type access struct {
	read  func(user uuid.UUID) *db.Queries
	begin func(ctx context.Context, user uuid.UUID) (pgx.Tx, error)
}

func (w *world) access(v string) access {
	if v == Baseline {
		q := db.New(w.app)
		return access{
			read:  func(uuid.UUID) *db.Queries { return q },
			begin: func(ctx context.Context, _ uuid.UUID) (pgx.Tx, error) { return w.app.Begin(ctx) },
		}
	}
	s := dbscope.New(w.app)
	return access{read: s.Reader, begin: s.Begin}
}

// do runs operation op for user and returns a number that depends on what came back (row
// counts), used to compare the variants' answers. r picks among the user's rows.
func (w *world) do(ctx context.Context, a access, op string, user uuid.UUID, r *rand.Rand) (int, error) {
	groups := w.userGroups[user]
	g := groups[r.IntN(len(groups))]
	switch op {
	case "list_groups":
		rows, err := a.read(user).ListGroupsForUser(ctx, db.ListGroupsForUserParams{UserID: user, PageSize: 100})
		return len(rows), err
	case "list_expenses":
		q := a.read(user)
		if _, err := q.GetGroupForMember(ctx, db.GetGroupForMemberParams{ID: g, UserID: user}); err != nil {
			return 0, err
		}
		rows, err := q.ListExpensesForMember(ctx, db.ListExpensesForMemberParams{UserID: user, GroupID: g, PageSize: 100})
		return len(rows), err
	case "get_expense":
		ex := w.groupExp[g]
		e := ex[r.IntN(len(ex))]
		q := a.read(user)
		if _, err := q.GetExpenseForMember(ctx, db.GetExpenseForMemberParams{UserID: user, ExpenseID: e, GroupID: g}); err != nil {
			return 0, err
		}
		items, err := q.ListExpenseItems(ctx, e)
		return 1 + len(items), err
	case "balances":
		rows, err := a.read(user).GetGroupBalances(ctx, db.GetGroupBalancesParams{CallerID: user, GroupID: g})
		sum := 0
		for _, row := range rows {
			if row.BalanceMinor < 0 {
				sum -= int(row.BalanceMinor)
			} else {
				sum += int(row.BalanceMinor)
			}
		}
		return len(rows)*1_000_000 + sum, err
	case "claim_unclaim":
		cl := w.groupClm[g]
		if len(cl) == 0 {
			return 0, nil
		}
		c := cl[r.IntN(len(cl))]
		return w.claimUnclaim(ctx, a, user, g, c)
	}
	return 0, fmt.Errorf("unknown operation %q", op)
}

// claimUnclaim is what the claim and unclaim handlers do, back to back: lock the expense
// row, check the item, add the user's claim, then remove it, each in its own transaction.
// The net effect on the data is nothing.
func (w *world) claimUnclaim(ctx context.Context, a access, user, group uuid.UUID, c claimable) (int, error) {
	one := func(add bool) (int, error) {
		tx, err := a.begin(ctx, user)
		if err != nil {
			return 0, err
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		q := db.New(tx)
		if _, err := controls.LockExpenseForClaim(ctx, q, db.LockExpenseForClaimParams{UserID: user, ExpenseID: c.expense, GroupID: group}); err != nil {
			return 0, err
		}
		if _, err := q.GetExpenseItem(ctx, db.GetExpenseItemParams{ID: c.item, ExpenseID: c.expense}); err != nil {
			return 0, err
		}
		var n int64
		if add {
			n, err = q.AddClaim(ctx, db.AddClaimParams{ItemID: c.item, ExpenseID: c.expense, GroupID: group, MemberID: user})
		} else {
			n, err = q.RemoveClaim(ctx, db.RemoveClaimParams{ItemID: c.item, ExpenseID: c.expense, MemberID: user})
		}
		if err != nil {
			return 0, err
		}
		return int(n), tx.Commit(ctx)
	}
	a1, err := one(true)
	if err != nil {
		return 0, err
	}
	a2, err := one(false)
	return a1*10 + a2, err
}

// measure runs one variant: the fixed questions, then warm-up, then the timed phase.
func (w *world) measure(ctx context.Context, p Params, v string, rep int) (VariantResult, error) {
	res := VariantResult{Variant: v, ByOp: map[string]Stat{}}
	a := w.access(v)

	// What the catalog and an unguarded query say about this variant.
	if err := w.owner.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relrowsecurity AND c.relforcerowsecurity`).Scan(&res.TablesWithRLS); err != nil {
		return res, err
	}
	want := 0
	if v == PoliciesOn {
		want = len(protectedTables)
	}
	if res.TablesWithRLS != want {
		return res, fmt.Errorf("%d tables have row-level security forced, want %d: the switch did not take effect", res.TablesWithRLS, want)
	}
	if err := func() error {
		tx, err := dbscope.New(w.app).Begin(ctx, w.users[0])
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		return tx.QueryRow(ctx, `SELECT count(*) FROM expenses`).Scan(&res.UnguardedCount)
	}(); err != nil {
		return res, err
	}
	var all int
	if err := w.owner.QueryRow(ctx, `SELECT count(*) FROM expenses`).Scan(&all); err != nil {
		return res, err
	}
	if (v == PoliciesOn) == (res.UnguardedCount == all) || res.UnguardedCount == 0 {
		return res, fmt.Errorf("an unguarded count sees %d of %d expenses in variant %s: row-level security is not in the state the variant says", res.UnguardedCount, all, v)
	}

	// The fixed questions: a deterministic list, asked once, in order.
	cr := rand.New(rand.NewPCG(p.Seed, 7))
	for i := 0; i < 300; i++ {
		user := w.users[cr.IntN(len(w.users))]
		op := ops[i%len(ops)].name
		n, err := w.do(ctx, a, op, user, cr)
		if err != nil {
			return res, fmt.Errorf("fixed question %d (%s): %w", i, op, err)
		}
		res.CheckSignature = append(res.CheckSignature, n)
	}

	// The timed loop.
	type sample struct {
		op  string
		d   time.Duration
		bad bool
	}
	var (
		mu      sync.Mutex
		samples []sample
	)
	start := time.Now()
	measureFrom := start.Add(p.Warmup)
	end := measureFrom.Add(p.Measure)
	var wg sync.WaitGroup
	for k := 0; k < p.Workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(p.Seed+uint64(rep)*1000+uint64(k), 11))
			var local []sample
			for time.Now().Before(end) {
				op := pick(r)
				user := w.users[r.IntN(len(w.users))]
				t0 := time.Now()
				_, err := w.do(ctx, a, op, user, r)
				d := time.Since(t0)
				if t0.After(measureFrom) {
					local = append(local, sample{op, d, err != nil})
				}
			}
			mu.Lock()
			samples = append(samples, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := end.Sub(measureFrom)
	res.ElapsedMeasureMs = float64(elapsed.Milliseconds())

	byOp := map[string][]time.Duration{}
	var all2 []time.Duration
	fails := map[string]int{}
	for _, s := range samples {
		if s.bad {
			fails[s.op]++
			continue
		}
		byOp[s.op] = append(byOp[s.op], s.d)
		all2 = append(all2, s.d)
	}
	for _, o := range ops {
		res.ByOp[o.name] = summarize(byOp[o.name], elapsed, fails[o.name])
	}
	totalFails := 0
	for _, f := range fails {
		totalFails += f
	}
	res.Total = summarize(all2, elapsed, totalFails)
	if totalFails > 0 {
		return res, fmt.Errorf("%d operations failed", totalFails)
	}
	return res, nil
}

func pick(r *rand.Rand) string {
	x := r.IntN(100)
	for _, o := range ops {
		if x < o.weight {
			return o.name
		}
		x -= o.weight
	}
	return ops[0].name
}

func summarize(d []time.Duration, elapsed time.Duration, failures int) Stat {
	st := Stat{N: len(d), Failures: failures}
	if len(d) == 0 {
		return st
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	rank := func(q float64) time.Duration { // nearest rank
		i := int(q*float64(len(d))+0.999999) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(d) {
			i = len(d) - 1
		}
		return d[i]
	}
	ms := func(x time.Duration) float64 { return float64(x.Microseconds()) / 1000 }
	st.P50Ms, st.P99Ms, st.MeanMs = ms(rank(0.50)), ms(rank(0.99)), ms(sum/time.Duration(len(d)))
	st.OpsPerS = float64(len(d)) / elapsed.Seconds()
	return st
}

// plans returns EXPLAIN ANALYZE text for a few queries run as a user with the policies on,
// to show how the policies are planned (in particular that my_group_ids() is evaluated once
// per statement and not once per row).
func (w *world) plans(ctx context.Context) (map[string]string, error) {
	user := w.users[0]
	g := w.userGroups[user][0]
	e := w.groupExp[g][0]
	queries := []struct {
		name, sql string
		args      []any
	}{
		{"all_expenses_unfiltered", `SELECT count(*) FROM expenses`, nil},
		{"all_ledger_entries_unfiltered", `SELECT count(*) FROM ledger_entries`, nil},
		{"items_of_one_expense", `SELECT id FROM expense_items WHERE expense_id = $1`, []any{e}},
	}
	out := map[string]string{}
	for _, q := range queries {
		tx, err := dbscope.New(w.app).Begin(ctx, user)
		if err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF, BUFFERS OFF) `+q.sql, q.args...)
		if err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		var text string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				_ = tx.Rollback(ctx)
				return nil, err
			}
			text += line + "\n"
		}
		rows.Close()
		_ = tx.Rollback(ctx)
		if rows.Err() != nil {
			return nil, rows.Err()
		}
		out[q.name] = text
	}
	return out, nil
}
