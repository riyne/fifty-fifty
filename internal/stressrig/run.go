package stressrig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/mockpay"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/settlerig"
)

// Params describes one stress run. Settlement is simulated: the provider is a mock.
type Params struct {
	OwnerURL, AppURL string // for the scratch database (the owner creates it)
	Bins             settlerig.Bins

	Seed     uint64
	Duration time.Duration

	// Expense side: Users spread over Groups groups, each user acting in a loop, plus
	// RaceLoops barrier-released race rounds per group.
	Users, Groups, RaceLoops int
	// Settle side: SettleWorkers goroutines running settle rounds (a fresh group per
	// round, RoundPause apart). 0 skips settlement: no workers or provider are started.
	SettleWorkers int
	RoundPause    time.Duration

	// The real worker processes and the mock provider.
	Workers, WorkerConcurrency int
	Lease, Poll                time.Duration
	MockFlags                  []string // mockpay flags; the webhook url and seed are added
	TimeoutHold                time.Duration
	WebhookSecret              string
	ResolveTimeout             time.Duration

	// Controls names protections to switch off (see internal/controls), in this process and
	// in the workers. Needs a binary and Bins built with the unsafe_controls tag; Run
	// refuses otherwise. DropPostingIndex also drops the unique index on a settlement's
	// ledger posting in the scratch database, the last layer behind Resolve's lock and
	// guards. Both are for negative-control runs only.
	Controls         []string
	DropPostingIndex bool

	APIPoolConns int // connections the api may use (default 30)
	Logf         func(format string, args ...any)
}

// MockLatency is the mock payment provider's simulated median latency in a stress run.
const MockLatency = "100ms"

// DefaultMockFlags is a provider that misbehaves a little in every way the settlement
// code handles, with webhooks on.
var DefaultMockFlags = []string{
	"--latency-median", MockLatency,
	"--rate-decline", "0.05", "--rate-timeout-before", "0.03", "--rate-5xx-before", "0.03",
	"--rate-timeout-after", "0.03", "--rate-5xx-after", "0.03", "--rate-read-error", "0.1",
	"--webhook-drop", "0.2", "--webhook-duplicate", "0.3", "--webhook-delay-max", "200ms", "--webhook-out-of-order", "0.3",
}

// ResolveRaceMockFlags makes more settlements resolve from two places at once: most
// webhook events arrive twice and soon, so a duplicate, the worker's own completion and
// reconcile meet in Resolve far more often than with DefaultMockFlags. For the
// resolve_* negative controls.
var ResolveRaceMockFlags = []string{
	"--latency-median", MockLatency,
	"--rate-decline", "0.05", "--rate-timeout-after", "0.05", "--rate-5xx-after", "0.05", "--rate-read-error", "0.1",
	"--webhook-duplicate", "0.8", "--webhook-delay-max", "120ms", "--webhook-out-of-order", "0.3",
}

// LockWaits is what a sampler saw of the database while the workload ran: direct
// evidence that requests really waited for each other's row locks.
type LockWaits struct {
	Samples        int `json:"samples"`
	SamplesWaiting int `json:"samples_with_a_backend_waiting_for_a_lock"`
	MaxWaiting     int `json:"max_backends_waiting"`
}

// Result is everything a run observed.
type Result struct {
	Report     Report            `json:"-"`
	Counts     map[string]int64  `json:"counts"`
	Unexpected []string          `json:"unexpected_responses,omitempty"`
	Locks      LockWaits         `json:"lock_waits"`
	Provider   *mockpay.Stats    `json:"provider,omitempty"`
	Unresolved int               `json:"settlements_unresolved"`
	Checked    Checked           `json:"checked"`
	Violations map[Invariant]int `json:"violations"`
	Examples   []Anomaly         `json:"violation_examples,omitempty"`
	Elapsed    float64           `json:"elapsed_seconds"`
}

// RaceMixedFloor is the least number of race rounds in which a finalize must split a
// burst of claims (some accepted, some refused) for a run with the claim and finalize
// locks on to count as having raced them. See the comment at its use for how it was chosen.
const RaceMixedFloor = 1

// Vacuous lists the dangerous situations the run was supposed to create and did not. A
// clean report from a run that never retried, never raced and never waited for a lock
// proves nothing, so a run with any entry here must not be reported as passing.
func (r Result) Vacuous(p Params) []string {
	var out []string
	need := func(key, why string) {
		if r.Counts[key] == 0 {
			out = append(out, fmt.Sprintf("%s never happened (%s)", key, why))
		}
	}
	if !slices.Contains(p.Controls, "idempotency_check") { // with the check off nothing is ever replayed
		need("replayed/create", "no retry or duplicate of a create was answered from the stored response")
		need("duplicate_waited_and_replayed/create", "no concurrent duplicate waited on the first and replayed it")
	}
	need("abandoned/create", "no request was abandoned by the client")
	need("claim/409_not_claimable", "no claim was refused for arriving after finalize")
	need("finalize/200", "no finalize succeeded")
	need("void/200", "no void succeeded")
	if p.RaceLoops > 0 {
		need("race_rounds", "no race round ran")
		need("race_double_finalize_refused", "two simultaneous finalizes never met")
		// "Refused for arriving after the finalize" is the claim lock working, and with
		// the claim lock off nothing is refused; with the finalize lock off the finalize
		// does not wait for claims. Either way the count is low by construction, so the
		// requirement does not apply. The floor is 1 ("it happened at all") on purpose: this
		// counts a rare event (2 to 15 per run, mean about 9, in the 30 recorded runs with both
		// locks on in stress-production-20261106T220956Z and stress-controls-20261106T221435Z),
		// so a floor above 1 fires by chance. It was 10, which flagged an unused recording
		// (stress-production-20261106T220029Z), then 5, which flagged 3 of the 33 control runs
		// of the recording that is used (counts 2, 3 and 4).
		if !slices.Contains(p.Controls, "claim_lock") && !slices.Contains(p.Controls, "finalize_lock") && r.Counts["race_mixed"] < RaceMixedFloor {
			out = append(out, fmt.Sprintf("race_mixed = %d, below %d (too few finalizes landed in the middle of a burst of claims)", r.Counts["race_mixed"], RaceMixedFloor))
		}
		// Two edits from one version meet and one is refused: only the version check and
		// the edit lock make that happen, so it is not required when either is off.
		editChecks := !slices.Contains(p.Controls, "edit_version_check") && !slices.Contains(p.Controls, "edit_lock")
		if editChecks {
			need("race_edit_conflict", "two edits made from one version never met, one applied and one refused")
		}
		if r.Counts["race_edit_rounds"] == 0 {
			out = append(out, "race_edit_rounds never happened (no edit race round ran)")
		}
	}
	need("edit/200", "no edit was accepted")
	if r.Locks.SamplesWaiting == 0 {
		out = append(out, "no sample ever saw a backend waiting for a lock")
	}
	if p.SettleWorkers > 0 {
		need("settle/202", "no settlement was accepted")
		need("refused/settle", "no settlement was refused for exceeding what the sender owes")
		if !slices.Contains(p.Controls, "idempotency_check") {
			need("replayed/settle", "no settlement retry was answered from the stored response")
		}
		if r.Counts["settlements_succeeded"] == 0 {
			out = append(out, "no settlement reached succeeded")
		}
	}
	return out
}

// Clean reports whether the run can be called a pass: every invariant held, no response
// was outside the documented ones, no server error, nothing unresolved, and the run was
// not vacuous.
func (r Result) Clean(p Params) bool {
	return r.Report.OK() && len(r.Unexpected) == 0 && r.Counts["http_5xx"] == 0 && r.Unresolved == 0 &&
		len(r.ServerErrors()) == 0 && len(r.Vacuous(p)) == 0
}

// ServerErrors lists the errors the api logged that are not a client going away. In the
// production build there should be none.
func (r Result) ServerErrors() []string {
	var out []string
	for _, k := range Keys(r.Counts) {
		if strings.HasPrefix(k, LogErrorPrefix) && k != LogErrorPrefix+"client_gone" && k != LogErrorPrefix+"client_gone_io_timeout" {
			out = append(out, fmt.Sprintf("%s=%d", k, r.Counts[k]))
		}
	}
	return out
}

// LogErrorPrefix starts the name of every counter of an error the api logged.
const LogErrorPrefix = "server_log_error/"

// WorkerLogErrorPrefix starts the name of every counter of an error a worker process
// logged. Unlike LogErrorPrefix counters these do not make a run unclean.
const WorkerLogErrorPrefix = "worker_log_error/"

var volatileRE = regexp.MustCompile(`127\.0\.0\.1:\d+|\[::1\]:\d+|\d+(\.\d+)?(ms|µs|s)\b`)

// workerLogErrors counts the WARN and ERROR lines of a worker's JSON log by cause (a
// failed attempt that will be retried is logged at WARN, only the last attempt at ERROR),
// each class starting with the level.
func workerLogErrors(log string) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(log, "\n") {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Err   string `json:"err"`
		}
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &rec) != nil || (rec.Level != "ERROR" && rec.Level != "WARN") {
			continue
		}
		out[rec.Level+" "+volatileRE.ReplaceAllString(classifyLogError(rec.Msg, rec.Err), "<v>")]++
	}
	return out
}

// errCapture is a slog handler that counts errors by cause and prints nothing.
type errCapture struct{ stats *Stats }

var constraintRE = regexp.MustCompile(`violates ([a-z ]+?) constraint "([^"]+)"`)

var uuidRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// classifyLogError names the cause of a logged error so equal causes count together:
// "client_gone" (and "client_gone_io_timeout") for a request the client abandoned, "unique:<constraint>" (or check,
// foreign key...) for a constraint the database enforced, otherwise the message with
// ids replaced by <id> (a deadlock, a guarded update that moved no row, ...).
func classifyLogError(msg, errText string) string {
	if strings.Contains(errText, "context canceled") || strings.Contains(errText, "context deadline exceeded") {
		return "client_gone"
	}
	// A client that gives up while the api is writing to the database leaves the
	// connection with an expired deadline: an i/o timeout, not a database fault. It is
	// counted apart so it stays visible.
	if strings.Contains(errText, "i/o timeout") {
		return "client_gone_io_timeout"
	}
	if m := constraintRE.FindStringSubmatch(errText); m != nil {
		return m[1] + ":" + m[2]
	}
	return "other:" + msg + ": " + uuidRE.ReplaceAllString(errText, "<id>")
}

func (errCapture) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelError }
func (h errCapture) WithAttrs([]slog.Attr) slog.Handler         { return h }
func (h errCapture) WithGroup(string) slog.Handler              { return h }
func (h errCapture) Handle(_ context.Context, r slog.Record) error {
	var errText string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "err" || a.Key == "error" {
			errText = a.Value.String()
		}
		return true
	})
	class := classifyLogError(r.Message, errText)
	h.stats.Inc(LogErrorPrefix + class)
	return nil
}

// Run executes one stress run on a scratch database and returns what it saw. A non-nil
// error means the run could not be carried out; anomalies are in the Result.
func Run(ctx context.Context, p Params) (Result, error) {
	var res Result
	logf := p.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if p.APIPoolConns == 0 {
		p.APIPoolConns = 30
	}
	if p.Users < 2*p.Groups || p.Groups < 1 {
		return res, fmt.Errorf("need at least 2 users per group (%d users, %d groups)", p.Users, p.Groups)
	}
	start := time.Now()

	workerEnv, undo, err := applyControls(p.Controls)
	if err != nil {
		return res, err
	}
	defer undo()

	scratch, err := settlerig.NewScratch(ctx, p.OwnerURL, p.AppURL)
	if err != nil {
		return res, err
	}
	defer func() {
		if err := scratch.Drop(context.Background()); err != nil {
			logf("drop %s: %v", scratch.Name, err)
		}
	}()

	if p.DropPostingIndex {
		conn, err := pgx.Connect(ctx, scratch.OwnerURL)
		if err != nil {
			return res, err
		}
		_, err = conn.Exec(ctx, `DROP INDEX ledger_transactions_one_settlement_posting`)
		conn.Close(ctx)
		if err != nil {
			return res, fmt.Errorf("drop posting index: %w", err)
		}
	}

	cfg, err := pgxpool.ParseConfig(scratch.AppURL)
	if err != nil {
		return res, err
	}
	cfg.MaxConns = int32(p.APIPoolConns)
	apiPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return res, err
	}
	defer apiPool.Close()

	api, err := StartAPI(ctx, apiPool, p.WebhookSecret)
	if err != nil {
		return res, err
	}
	defer api.Close()

	env := &Env{API: api, Client: NewClient(api.URL), Pool: scratch.Pool, Stats: newStats(), Seed: p.Seed, prefix: uuid.NewString()[:8]}

	// The api logs every internal error through slog. Count them by cause instead of
	// printing thousands of lines: a request the client abandoned logs "context canceled",
	// which is expected, and anything else (a constraint violation, a failed update) is
	// the database or a guard refusing something the code tried, which is the evidence of
	// which layer caught a problem.
	prev := slog.Default()
	slog.SetDefault(slog.New(errCapture{env.Stats}))
	defer slog.SetDefault(prev)

	var mockURL string
	var workers []*settlerig.Proc
	if p.SettleWorkers > 0 {
		flags := append([]string{"--seed", strconv.FormatUint(p.Seed, 10), "--timeout-hold", p.TimeoutHold.String(),
			"--webhook-url", api.URL + settlements.WebhookPath}, p.MockFlags...)
		mock, url, err := settlerig.StartMockpay(p.Bins.Mockpay, p.WebhookSecret, flags...)
		if err != nil {
			return res, err
		}
		defer mock.Stop()
		mockURL = url
		for i := 1; i <= p.Workers; i++ {
			w, err := settlerig.StartWorker(settlerig.WorkerSpec{Bin: p.Bins.Worker, DatabaseURL: scratch.WorkerURL, PaymentsURL: mockURL,
				Name: fmt.Sprintf("worker-%d", i), Concurrency: p.WorkerConcurrency, Lease: p.Lease, Poll: p.Poll, Env: workerEnv})
			if err != nil {
				return res, err
			}
			defer w.Stop()
			workers = append(workers, w)
		}
		if len(p.Controls) > 0 {
			// A worker binary built without the tag would ignore the variable and the run
			// would report protections as disabled that were not. The tagged worker says so
			// at start-up; wait for that line.
			for _, w := range workers {
				if err := waitForLog(w, "protections OFF", 15*time.Second); err != nil {
					return res, fmt.Errorf("%s did not report the controls off (is Bins built with the unsafe_controls tag?): %w", w.Name, err)
				}
			}
		}
	}

	// Users and groups.
	var groups []*group
	per := p.Users / p.Groups
	for gi := 0; gi < p.Groups; gi++ {
		var us []User
		for i := 0; i < per; i++ {
			u, err := env.NewUser(ctx, fmt.Sprintf("u%d-%d", gi, i))
			if err != nil {
				return res, err
			}
			us = append(us, u)
		}
		id, err := env.NewGroup(ctx, us)
		if err != nil {
			return res, err
		}
		groups = append(groups, &group{id: id, users: us})
	}
	var settleUsers []User
	if p.SettleWorkers > 0 {
		for i := 0; i < SettleUsers; i++ {
			u, err := env.NewUser(ctx, fmt.Sprintf("s%d", i))
			if err != nil {
				return res, err
			}
			settleUsers = append(settleUsers, u)
		}
	}

	// The workload.
	runCtx, stop := context.WithTimeout(ctx, p.Duration)
	defer stop()
	sampler := startSampler(ctx, scratch.Pool)
	var wg sync.WaitGroup
	stream := uint64(0)
	for _, g := range groups {
		for i := range g.users {
			stream++
			wg.Add(1)
			go func() { defer wg.Done(); env.userLoop(runCtx, g, i, stream) }()
		}
		for i := 0; i < p.RaceLoops; i++ {
			stream++
			wg.Add(1)
			go func() { defer wg.Done(); env.raceLoop(runCtx, g, stream) }()
		}
	}
	for i := 0; i < p.SettleWorkers; i++ {
		stream++
		wg.Add(1)
		go func() {
			defer wg.Done()
			env.settleLoop(runCtx, settleUsers, stream, func() {
				select {
				case <-runCtx.Done():
				case <-time.After(p.RoundPause):
				}
			})
		}()
	}
	wg.Wait()
	res.Locks = sampler()
	logf("workload finished after %s", time.Since(start).Round(time.Second))

	// Let settlements finish, then late and duplicate webhooks.
	var provider []mockpay.Ledger
	exp := Expect{}
	if p.SettleWorkers > 0 {
		ids, err := settlementIDs(ctx, scratch.Pool)
		if err != nil {
			return res, err
		}
		if res.Unresolved, err = settlerig.WaitResolved(ctx, scratch.Pool, ids, p.ResolveTimeout); err != nil {
			return res, err
		}
		time.Sleep(2 * time.Second)
		if provider, err = settlerig.FetchProvider(ctx, mockURL); err != nil {
			return res, err
		}
		exp.Provider = &provider
		st, err := fetchProviderStats(ctx, mockURL)
		if err != nil {
			return res, err
		}
		res.Provider = &st
		var ok int64
		if err := scratch.Pool.QueryRow(ctx, `SELECT count(*) FROM settlements WHERE status = 'succeeded'`).Scan(&ok); err != nil {
			return res, err
		}
		env.Stats.Add("settlements_succeeded", ok)
	}

	env.mu.Lock()
	exp.Creates, exp.Frozen = env.creates, env.frozen
	env.mu.Unlock()
	if res.Report, err = Check(ctx, scratch.Pool, exp); err != nil {
		return res, err
	}
	res.Checked, res.Violations, res.Examples = res.Report.Checked, res.Report.Counts, res.Report.Examples
	// What the worker processes logged: the same causes as the api's, kept apart because
	// workers legitimately log some failures (a provider that timed out).
	for _, w := range workers {
		for class, n := range workerLogErrors(w.Log()) {
			env.Stats.Add(WorkerLogErrorPrefix+class, int64(n))
		}
	}
	res.Counts, res.Unexpected = env.Stats.Snapshot()
	res.Elapsed = time.Since(start).Seconds()
	return res, nil
}

func waitForLog(p *settlerig.Proc, substr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(p.Log(), substr) {
			return nil
		}
		select {
		case <-p.Done():
			return fmt.Errorf("exited: %s", p.Log())
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("no %q in its output after %s", substr, timeout)
}

func settlementIDs(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM settlements`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// startSampler polls pg_locks for lock requests that have not been granted, in this
// database only, every 20 ms. The returned function stops it and reports.
func startSampler(ctx context.Context, pool *pgxpool.Pool) func() LockWaits {
	var lw LockWaits
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			var n int
			err := pool.QueryRow(ctx, `
				SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
				WHERE NOT l.granted AND a.datname = current_database()`).Scan(&n)
			if err != nil {
				continue
			}
			lw.Samples++
			if n > 0 {
				lw.SamplesWaiting++
			}
			lw.MaxWaiting = max(lw.MaxWaiting, n)
		}
	}()
	return func() LockWaits { close(done); <-stopped; return lw }
}

func fetchProviderStats(ctx context.Context, baseURL string) (mockpay.Stats, error) {
	return settlerig.FetchStats(ctx, baseURL)
}

// Summary is a human-readable account of a run: the violations of every invariant (zeros
// included, because "the database's zero-sum rule held under this control" is a result),
// what the clients saw, and the errors the api logged, which show the layer that stopped
// something.
func (r Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "violations:")
	for _, inv := range Invariants {
		fmt.Fprintf(&b, " %s=%d", inv, r.Violations[inv])
	}
	fmt.Fprintf(&b, "\nclients saw: http_5xx=%d unexpected=%d settle/202=%d refused/settle=%d finalize/200=%d void/200=%d claim/409=%d",
		r.Counts["http_5xx"], len(r.Unexpected), r.Counts["settle/202"], r.Counts["refused/settle"], r.Counts["finalize/200"], r.Counts["void/200"], r.Counts["claim/409_not_claimable"])
	fmt.Fprintf(&b, "\nchecked: %+v; unresolved settlements: %d; settlements succeeded: %d", r.Checked, r.Unresolved, r.Counts["settlements_succeeded"])
	fmt.Fprintf(&b, "\nlock waits: %+v", r.Locks)
	b.WriteString("\napi logged errors:")
	n := 0
	for _, k := range Keys(r.Counts) {
		if strings.HasPrefix(k, LogErrorPrefix) {
			fmt.Fprintf(&b, "\n  %s = %d", strings.TrimPrefix(k, LogErrorPrefix), r.Counts[k])
			n++
		}
	}
	if n == 0 {
		b.WriteString(" none")
	}
	if r.Provider != nil {
		fmt.Fprintf(&b, "\nprovider: create_calls=%d payments=%d charges=%d", r.Provider.CreateCalls, r.Provider.Payments, r.Provider.Charges)
	}
	for _, a := range r.Examples {
		d := a.Detail
		if len(d) > 200 {
			d = d[:200] + "..."
		}
		fmt.Fprintf(&b, "\n  e.g. %s: %s", a.Invariant, d)
	}
	return b.String()
}
