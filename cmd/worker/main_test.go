package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/testutil"
	"github.com/riyne/fifty-fifty/internal/worker"
)

func TestParseRequeue(t *testing.T) {
	id1, id2 := uuid.NewString(), uuid.NewString()
	ok := map[string]struct {
		args []string
		kind string
		nIDs int
	}{
		"kind":         {[]string{"--kind", "k"}, "k", 0},
		"one id":       {[]string{"--id", id1}, "", 1},
		"repeated ids": {[]string{"--id", id1, "--id", id2}, "", 2},
		"kind and id":  {[]string{"--kind", "k", "--id", id1}, "k", 1},
	}
	for name, c := range ok {
		p, err := parseRequeue(c.args, &bytes.Buffer{})
		if err != nil || p.Kind != c.kind || len(p.IDs) != c.nIDs {
			t.Errorf("%s: %+v, %v", name, p, err)
		}
	}
	bad := map[string][]string{
		"nothing selected": {},
		"not a uuid":       {"--id", "nope"},
		"stray argument":   {"--kind", "k", "everything"},
		"unknown flag":     {"--all"},
	}
	for name, args := range bad {
		var out bytes.Buffer
		if _, err := parseRequeue(args, &out); !errors.Is(err, errUsage) {
			t.Errorf("%s: err = %v, want usage error", name, err)
		}
	}
}

func TestRun_RejectsOtherCommands(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{nil, {"frobnicate"}, {"run", "--nope"}, {"run", "extra"}} {
		if err := run(context.Background(), args, &out, &out); !errors.Is(err, errUsage) {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func TestRequeue_PrintsTheRequeuedIDs(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	kind := "test.cli." + uuid.NewString()

	var id uuid.UUID
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id, err = queue.Enqueue(ctx, tx, queue.NewJob{Kind: kind, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := queue.ClaimJobs(ctx, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: 1, Lease: time.Minute})
	if err != nil || len(c) != 1 {
		t.Fatal(c, err)
	}
	if st, err := queue.Fail(ctx, pool, c[0], queue.FailParams{Error: "x"}); err != nil || st != queue.StateDead {
		t.Fatal(st, err)
	}

	var out bytes.Buffer
	if err := requeue(ctx, pool, queue.RequeueParams{Kind: kind}, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != id.String() {
		t.Errorf("printed %q, want %q", got, id)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM jobs WHERE id = $1`, id).Scan(&state); err != nil || state != queue.StateQueued {
		t.Errorf("state = %q, %v", state, err)
	}
	// Nothing left to requeue: prints nothing.
	out.Reset()
	if err := requeue(ctx, pool, queue.RequeueParams{Kind: kind}, &out); err != nil || out.Len() != 0 {
		t.Errorf("second run printed %q, %v", out.String(), err)
	}
}

func TestParseRun(t *testing.T) {
	cfg, addr, err := parseRun([]string{"--concurrency", "8", "--lease", "10s", "--poll", "250ms", "--grace", "5s", "--metrics-addr", "127.0.0.1:9102"}, &bytes.Buffer{})
	if err != nil || addr != "127.0.0.1:9102" || cfg.Concurrency != 8 || cfg.Lease != 10*time.Second || cfg.PollInterval != 250*time.Millisecond || cfg.ShutdownGrace != 5*time.Second {
		t.Errorf("%+v, %v", cfg, err)
	}
	cfg, addr, err = parseRun(nil, &bytes.Buffer{})
	if err != nil || addr != "" || cfg.Concurrency != 4 || cfg.Lease != 30*time.Second {
		t.Errorf("defaults: %+v, %v", cfg, err)
	}
}

// Every kind the binary can run has a handler, and the periodic schedule only names
// kinds that have one (worker.Run refuses otherwise).
func TestHandlersCoverTheSchedule(t *testing.T) {
	h, err := handlers(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, sched := range []queue.PeriodicJob{worker.CleanupSchedule, worker.PruneSchedule} {
		if _, ok := h[sched.Kind]; !ok {
			t.Errorf("no handler for the scheduled kind %q", sched.Kind)
		}
	}
}

// Settlement execution is registered only with a provider to talk to, and a bad URL
// stops the worker at startup instead of failing every settlement later.
func TestHandlers_SettlementExecutionNeedsAProvider(t *testing.T) {
	h, err := handlers(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{settlements.KindExecute, settlements.KindReconcile, settlements.KindWatchdog} {
		if _, ok := h[k]; ok {
			t.Errorf("%s is registered without PAYMENTS_URL", k)
		}
	}
	h, err = handlers(nil, "http://127.0.0.1:8090")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{settlements.KindExecute, settlements.KindReconcile, settlements.KindWatchdog} {
		if _, ok := h[k]; !ok {
			t.Errorf("%s is not registered with PAYMENTS_URL set", k)
		}
	}
	// The watchdog is periodic, so worker.Run needs a handler for its kind (it
	// refuses a schedule without one): the schedule must name a registered kind.
	if _, ok := h[settlements.WatchdogSchedule.Kind]; !ok {
		t.Error("the watchdog schedule names a kind with no handler")
	}
	if _, err := handlers(nil, "127.0.0.1:8090"); err == nil {
		t.Error("a PAYMENTS_URL without a scheme was accepted")
	}
}

func TestServeMetrics_ServesOnlyMetricsAndStopsWithTheContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveMetrics(ctx, ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "up 1\n") }))
	}()

	base := "http://" + ln.Addr().String()
	get := func(path string) (int, string) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/metrics"); code != 200 || body != "up 1\n" {
		t.Errorf("/metrics: %d %q", code, body)
	}
	for _, p := range []string{"/", "/metrics/extra", "/debug/pprof/"} {
		if code, _ := get(p); code != 404 {
			t.Errorf("%s: %d, want 404", p, code)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the metrics server did not stop when its context was canceled")
	}
}
