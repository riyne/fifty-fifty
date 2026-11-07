package settlerig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/benchwork"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/mockpay"
	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/settlements"
)

// Mix is one fault configuration of the mock provider.
type Mix struct {
	Name string
	// Mock is mockpay's rate and webhook-behavior flags. The seed, the timeout hold and
	// the webhook URL are added by RunLoad.
	Mock []string
	// Webhooks: whether the provider is given the api's webhook URL.
	Webhooks bool
	// KillOne: SIGKILL the first worker when half the settlements have arrived.
	KillOne bool
}

// LoadParams is one benchmark run.
type LoadParams struct {
	OwnerURL, AppURL string
	Bins             Bins
	Mix              Mix
	Settlements      int
	Rate             float64 // arrivals per second (Poisson)
	Workers          int
	Concurrency      int
	Lease, Poll      time.Duration
	TimeoutHold      time.Duration // how long the provider holds a timed-out connection
	Seed             uint64        // seeds the arrivals and the provider
	WebhookSecret    string
	ResolveTimeout   time.Duration // how long to wait for every settlement to be final
	Logf             func(format string, args ...any)
}

// LoadResult is everything one run observed.
type LoadResult struct {
	Mix         string `json:"mix"`
	Seed        uint64 `json:"seed"`
	Settlements int    `json:"settlements"`

	ByStatus   map[string]int `json:"final_status_counts"`
	Unresolved int            `json:"unresolved_at_timeout"`
	Postings   int            `json:"ledger_postings"`
	Charges    int            `json:"provider_charges"`
	Anomalies  []string       `json:"anomalies"`

	// EndToEnd: seconds from the settlement row's creation to it becoming final.
	EndToEnd benchwork.Dist `json:"end_to_end_seconds"`
	// UnknownToFinal: for settlements that were recorded `unknown`, seconds from the
	// first such record to the settlement becoming final.
	UnknownToFinal benchwork.Dist `json:"unknown_to_final_seconds"`
	WentUnknown    int            `json:"settlements_that_went_unknown"`

	Provider mockpay.Stats `json:"provider_stats"`

	ExecuteJobsRetried int `json:"execute_jobs_with_more_than_one_attempt"`
	ReconcileJobs      int `json:"reconcile_jobs_created"`
	DeadJobs           int `json:"dead_jobs"`
	WatchdogRequeues   int `json:"watchdog_requeues"`

	KilledWorker     bool    `json:"killed_a_worker"`
	ArrivalSpanSecs  float64 `json:"arrival_span_seconds"`
	TotalSecs        float64 `json:"run_seconds_until_final"`
	DatabaseJobsRows int64   `json:"scratch_jobs_rows_at_start"`
}

// Arrivals returns n arrival offsets of a Poisson process with the given rate, from a
// generator seeded with seed: the same seed gives the same offsets.
func Arrivals(seed uint64, n int, rate float64) []time.Duration {
	rng := rand.New(rand.NewPCG(seed, 0x61727269766c73))
	out := make([]time.Duration, n)
	var t float64
	for i := range out {
		t += rng.ExpFloat64() / rate
		out[i] = time.Duration(t * float64(time.Second))
	}
	return out
}

// RunLoad runs one benchmark run on a scratch database: real worker processes, a real
// mock-provider process, the api's webhook handler in this process, settlements
// inserted as the API handler inserts them (the settlement, its execute job and the
// audit event in one transaction; the API's authentication and capacity check are not
// involved or measured), arriving as a Poisson process.
func RunLoad(ctx context.Context, p LoadParams) (LoadResult, error) {
	logf := p.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	res := LoadResult{Mix: p.Mix.Name, Seed: p.Seed, Settlements: p.Settlements}

	scratch, err := NewScratch(ctx, p.OwnerURL, p.AppURL)
	if err != nil {
		return res, err
	}
	defer func() {
		if err := scratch.Drop(context.Background()); err != nil {
			logf("drop %s: %v", scratch.Name, err)
		}
	}()
	// The harness seeds, inserts settlements as the create handler does, and checks, with the
	// owner connection: it sees every row whatever row-level security does to the roles
	// under test. The webhook handler below runs as fifty_system, as in the api.
	pool := scratch.Owner
	sysPool, err := pgxpool.New(ctx, scratch.SystemURL)
	if err != nil {
		return res, fmt.Errorf("system pool: %w", err)
	}
	defer sysPool.Close()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&res.DatabaseJobsRows); err != nil {
		return res, err
	}

	// The api's webhook endpoint, served from this process on the scratch database.
	wh, err := settlements.NewWebhookHandler(sysPool, []byte(p.WebhookSecret))
	if err != nil {
		return res, err
	}
	mux := http.NewServeMux()
	mux.Handle("POST "+settlements.WebhookPath, wh)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return res, err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	flags := append([]string{"--seed", strconv.FormatUint(p.Seed, 10), "--timeout-hold", p.TimeoutHold.String()}, p.Mix.Mock...)
	if p.Mix.Webhooks {
		flags = append(flags, "--webhook-url", "http://"+ln.Addr().String()+settlements.WebhookPath)
	}
	mock, mockURL, err := StartMockpay(p.Bins.Mockpay, p.WebhookSecret, flags...)
	if err != nil {
		return res, err
	}
	defer mock.Stop()

	payer, debtors, group, err := seedGroup(ctx, pool, 20)
	if err != nil {
		return res, err
	}

	var workers []*Proc
	defer func() {
		for _, w := range workers {
			w.Stop()
		}
	}()
	for i := 1; i <= p.Workers; i++ {
		w, err := StartWorker(WorkerSpec{Bin: p.Bins.Worker, DatabaseURL: scratch.WorkerURL, PaymentsURL: mockURL,
			Name: fmt.Sprintf("worker-%d", i), Concurrency: p.Concurrency, Lease: p.Lease, Poll: p.Poll})
		if err != nil {
			return res, err
		}
		workers = append(workers, w)
	}

	// Arrivals.
	offsets := Arrivals(p.Seed, p.Settlements, p.Rate)
	q := db.New(pool)
	ids := make([]uuid.UUID, 0, p.Settlements)
	begin := time.Now()
	for i, off := range offsets {
		if d := time.Until(begin.Add(off)); d > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(d):
			}
		}
		id, err := insertSettlement(ctx, pool, q, group, debtors[i%len(debtors)], payer)
		if err != nil {
			return res, fmt.Errorf("insert settlement %d: %w", i, err)
		}
		ids = append(ids, id)
		if p.Mix.KillOne && i == p.Settlements/2 {
			if err := workers[0].Kill(); err != nil {
				return res, err
			}
			res.KilledWorker = true
			logf("%s: killed %s after %d arrivals", p.Mix.Name, workers[0].Name, i+1)
		}
	}
	res.ArrivalSpanSecs = time.Since(begin).Seconds()

	left, err := WaitResolved(ctx, pool, ids, p.ResolveTimeout)
	if err != nil {
		return res, err
	}
	res.Unresolved = left
	res.TotalSecs = time.Since(begin).Seconds()
	time.Sleep(2 * time.Second) // duplicate and late webhooks still arriving

	provider, err := FetchProvider(ctx, mockURL)
	if err != nil {
		return res, err
	}
	rep, err := Verify(ctx, pool, provider, ids)
	if err != nil {
		return res, err
	}
	res.ByStatus, res.Postings, res.Charges, res.Anomalies = rep.ByStatus, rep.Postings, rep.ProviderCharges, rep.Anomalies
	if res.Provider, err = FetchStats(ctx, mockURL); err != nil {
		return res, err
	}
	if err := timings(ctx, pool, &res); err != nil {
		return res, err
	}
	return res, nil
}

func seedGroup(ctx context.Context, pool *pgxpool.Pool, debtors int) (payer uuid.UUID, ds []uuid.UUID, group uuid.UUID, err error) {
	user := func() (id uuid.UUID, err error) {
		err = pool.QueryRow(ctx, `INSERT INTO users (issuer, subject) VALUES ('bench', $1) RETURNING id`, uuid.NewString()).Scan(&id)
		return
	}
	if payer, err = user(); err != nil {
		return
	}
	if err = pool.QueryRow(ctx, `INSERT INTO groups (name, currency, created_by) VALUES ('bench', 'CAD', $1) RETURNING id`, payer).Scan(&group); err != nil {
		return
	}
	if _, err = pool.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'owner')`, group, payer); err != nil {
		return
	}
	for range debtors {
		var d uuid.UUID
		if d, err = user(); err != nil {
			return
		}
		if _, err = pool.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'member')`, group, d); err != nil {
			return
		}
		ds = append(ds, d)
	}
	return
}

// insertSettlement does what the create-settlement handler does inside its idempotent
// transaction, minus the group lock and capacity check: the settlement, its execute
// job and the audit event, in one transaction.
func insertSettlement(ctx context.Context, pool *pgxpool.Pool, q *db.Queries, group, from, to uuid.UUID) (uuid.UUID, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	s, err := q.WithTx(tx).CreateSettlement(ctx, db.CreateSettlementParams{GroupID: group, FromMember: from, ToMember: to, AmountMinor: 700})
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: settlements.KindExecute, Payload: settlements.ExecutePayload{SettlementID: s.ID}}); err != nil {
		return uuid.Nil, err
	}
	if err := q.WithTx(tx).InsertAuditEvent(ctx, db.InsertAuditEventParams{ActorUserID: from, GroupID: group, Kind: "settlement.created", Data: []byte(`{}`)}); err != nil {
		return uuid.Nil, err
	}
	return s.ID, tx.Commit(ctx)
}

// FetchStats reads the mock provider's own counters.
func FetchStats(ctx context.Context, baseURL string) (mockpay.Stats, error) {
	var s mockpay.Stats
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/stats", nil)
	if err != nil {
		return s, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	return s, decodeJSON(resp.Body, &s)
}

// timings reads the run's durations and job counts from Postgres, all on the database clock.
func timings(ctx context.Context, pool *pgxpool.Pool, res *LoadResult) error {
	floats := func(sql string) ([]float64, error) {
		rows, err := pool.Query(ctx, sql)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []float64
		for rows.Next() {
			var x float64
			if err := rows.Scan(&x); err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, rows.Err()
	}
	e2e, err := floats(`SELECT extract(epoch FROM resolved_at - created_at)::float8 FROM settlements WHERE resolved_at IS NOT NULL`)
	if err != nil {
		return err
	}
	res.EndToEnd = benchwork.Distribution(e2e)
	unk, err := floats(`
		SELECT extract(epoch FROM s.resolved_at - u.first)::float8
		FROM settlements s
		JOIN (SELECT data->>'settlement_id' AS sid, min(created_at) AS first FROM audit_events WHERE kind = 'settlement.unknown' GROUP BY 1) u
		  ON u.sid = s.id::text
		WHERE s.resolved_at IS NOT NULL`)
	if err != nil {
		return err
	}
	res.UnknownToFinal, res.WentUnknown = benchwork.Distribution(unk), len(unk)
	return pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM jobs WHERE kind = 'settlement.execute' AND attempts > 1)::int,
		       (SELECT count(*) FROM jobs WHERE kind = 'settlement.reconcile')::int,
		       (SELECT count(*) FROM jobs WHERE state = 'dead')::int,
		       (SELECT count(*) FROM audit_events WHERE kind = 'settlement.execute_requeued')::int`).
		Scan(&res.ExecuteJobsRetried, &res.ReconcileJobs, &res.DeadJobs, &res.WatchdogRequeues)
}

func decodeJSON(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }
