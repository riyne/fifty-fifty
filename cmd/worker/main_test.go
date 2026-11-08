package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/queue"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
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

func TestParseDeleteDead(t *testing.T) {
	id := uuid.NewString()
	ok := map[string]struct {
		args   []string
		kind   string
		nIDs   int
		age    time.Duration
		dryRun bool
	}{
		"kind":             {[]string{"--kind", "k"}, "k", 0, 0, false},
		"an id":            {[]string{"--id", id}, "", 1, 0, false},
		"an age":           {[]string{"--older-than", "720h"}, "", 0, 720 * time.Hour, false},
		"kind and age":     {[]string{"--kind", "k", "--older-than", "24h"}, "k", 0, 24 * time.Hour, false},
		"a dry run":        {[]string{"--dry-run", "--kind", "k"}, "k", 0, 0, true},
		"all of them":      {[]string{"--kind", "k", "--id", id, "--older-than", "1h", "--dry-run"}, "k", 1, time.Hour, true},
		"age with seconds": {[]string{"--older-than", "90s"}, "", 0, 90 * time.Second, false},
	}
	for name, c := range ok {
		p, err := parseDeleteDead(c.args, &bytes.Buffer{})
		if err != nil || p.Kind != c.kind || len(p.IDs) != c.nIDs || p.OlderThan != c.age || p.DryRun != c.dryRun {
			t.Errorf("%s: %+v, %v", name, p, err)
		}
	}
	// No form deletes everything: with no filter, however it is spelled, it is a usage error.
	bad := map[string][]string{
		"nothing":                 {},
		"only a dry run":          {"--dry-run"},
		"a zero age":              {"--older-than", "0s"},
		"a negative age":          {"--older-than", "-1h"},
		"not a duration":          {"--older-than", "a while"},
		"not a uuid":              {"--id", "nope"},
		"a stray argument":        {"--kind", "k", "everything"},
		"an all flag that is not": {"--all"},
	}
	for name, args := range bad {
		var out bytes.Buffer
		if _, err := parseDeleteDead(args, &out); !errors.Is(err, errUsage) {
			t.Errorf("%s: err = %v, want a usage error", name, err)
		}
	}
}

func TestDeleteDead_DryRunPrintsAndDeletesNothingThenTheRealRunDeletes(t *testing.T) {
	pool := testutil.WorkerPool(t)
	ctx := context.Background()
	kind := "test.cli.delete-dead." + uuid.NewString()

	for range 2 {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := queue.Enqueue(ctx, tx, queue.NewJob{Kind: kind, MaxAttempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		c, err := queue.ClaimJobs(ctx, pool, queue.ClaimParams{Kinds: []string{kind}, Limit: 1, Lease: time.Minute})
		if err != nil || len(c) != 1 || c[0].ID != id {
			t.Fatal(c, err)
		}
		if st, err := queue.Fail(ctx, pool, c[0], queue.FailParams{Error: "x"}); err != nil || st != queue.StateDead {
			t.Fatal(st, err)
		}
	}
	count := func() (n int) {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = $1`, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	lines := func(out *bytes.Buffer) []string { return strings.Fields(out.String()) }

	var out bytes.Buffer
	if err := deleteDead(ctx, pool, queue.DeleteDeadParams{Kind: kind, DryRun: true}, &out); err != nil {
		t.Fatal(err)
	}
	dry := lines(&out)
	if len(dry) != 2 || count() != 2 {
		t.Fatalf("dry run printed %v and left %d jobs, want 2 ids and 2 jobs", dry, count())
	}

	out.Reset()
	if err := deleteDead(ctx, pool, queue.DeleteDeadParams{Kind: kind}, &out); err != nil {
		t.Fatal(err)
	}
	if got := lines(&out); !slices.Equal(got, dry) || count() != 0 {
		t.Errorf("the real run printed %v and left %d jobs, want %v and none", got, count(), dry)
	}
}

// A delete-dead with no filter is refused before any database is needed: nothing in this test
// has DATABASE_URL, so reaching the connection would be a different error.
func TestRun_DeleteDeadWithoutAFilterIsAUsageError(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	var out bytes.Buffer
	for _, args := range [][]string{{"delete-dead"}, {"delete-dead", "--dry-run"}, {"delete-dead", "--older-than", "0s"}} {
		if err := run(context.Background(), args, &out, &out); !errors.Is(err, errUsage) {
			t.Errorf("%v: err = %v, want a usage error", args, err)
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
	pool := testutil.WorkerPool(t) // the queue is the worker role's: fifty_app cannot claim or fail a job (00017)
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
	h, err := handlers(nil, "", receiptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sched := range []queue.PeriodicJob{worker.CleanupSchedule, worker.PruneSchedule, receiptparse.SweepSchedule} {
		if _, ok := h[sched.Kind]; !ok {
			t.Errorf("no handler for the scheduled kind %q", sched.Kind)
		}
	}
}

// Settlement execution is registered only with a provider to talk to, and a bad URL
// stops the worker at startup instead of failing every settlement later.
func TestHandlers_SettlementExecutionNeedsAProvider(t *testing.T) {
	h, err := handlers(nil, "", receiptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{settlements.KindExecute, settlements.KindReconcile, settlements.KindWatchdog} {
		if _, ok := h[k]; ok {
			t.Errorf("%s is registered without PAYMENTS_URL", k)
		}
	}
	h, err = handlers(nil, "http://127.0.0.1:8090", receiptConfig{})
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
	if _, err := handlers(nil, "127.0.0.1:8090", receiptConfig{}); err == nil {
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

// ---- receipt.parse registration ----

func fakeTesseract(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tesseract")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// The worker reads receipts only when it has the image directory and a model; a half
// configuration leaves the jobs for a complete worker, and a complete one that cannot
// work refuses to start instead of failing every receipt later.
func TestHandlers_ReceiptParseNeedsTheWholeConfiguration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	tess := fakeTesseract(t)
	registered := func(rc receiptConfig) bool {
		t.Helper()
		h, err := handlers(nil, "", rc)
		if err != nil {
			t.Fatalf("%+v: %v", rc, err)
		}
		_, sweep := h[receiptparse.KindSweep]
		if !sweep {
			t.Error("the sweep must always be registered")
		}
		_, ok := h[receiptparse.KindParse]
		return ok
	}
	for name, rc := range map[string]receiptConfig{
		"nothing":                 {},
		"directory only":          {Dir: dir},
		"model only (mock)":       {BaseURL: "http://127.0.0.1:8089"},
		"key only":                {APIKey: "sk-ant-x"},
		"model without directory": {APIKey: "sk-ant-x", TesseractPath: tess},
	} {
		if registered(rc) {
			t.Errorf("%s: receipt.parse registered", name)
		}
	}
	for name, rc := range map[string]receiptConfig{
		"directory and a key":        {Dir: dir, APIKey: "sk-ant-x", TesseractPath: tess},
		"directory and the mock":     {Dir: dir, BaseURL: "http://127.0.0.1:8089", TesseractPath: tess},
		"with a model override":      {Dir: dir, APIKey: "sk-ant-x", Model: "claude-sonnet-5-5", TesseractPath: tess},
		"key and a custom https url": {Dir: dir, APIKey: "sk-ant-x", BaseURL: "https://proxy.example.com", TesseractPath: tess},
	} {
		if !registered(rc) {
			t.Errorf("%s: receipt.parse not registered", name)
		}
	}
}

func TestHandlers_AConfiguredButBrokenReceiptSetupStopsTheWorker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	tess := fakeTesseract(t)
	for name, rc := range map[string]receiptConfig{
		"tesseract missing":           {Dir: dir, APIKey: "sk-ant-x", TesseractPath: filepath.Join(t.TempDir(), "nope")},
		"plain http to a remote host": {Dir: dir, APIKey: "sk-ant-x", BaseURL: "http://example.com", TesseractPath: tess},
		"key with a space":            {Dir: dir, APIKey: "sk ant", TesseractPath: tess},
		"remote host and no key":      {Dir: dir, BaseURL: "https://proxy.example.com", TesseractPath: tess},
		"directory is a file":         {Dir: tess, APIKey: "sk-ant-x", TesseractPath: tess},
	} {
		if _, err := handlers(nil, "", rc); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A world-readable image directory is refused (receipts are private).
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := handlers(nil, "", receiptConfig{Dir: open, APIKey: "sk-ant-x", TesseractPath: tess}); err == nil {
		t.Error("a world-readable image directory was accepted")
	}
}
