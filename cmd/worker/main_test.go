package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/queue"
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
	cfg, err := parseRun([]string{"--concurrency", "8", "--lease", "10s", "--poll", "250ms", "--grace", "5s"}, &bytes.Buffer{})
	if err != nil || cfg.Concurrency != 8 || cfg.Lease != 10*time.Second || cfg.PollInterval != 250*time.Millisecond || cfg.ShutdownGrace != 5*time.Second {
		t.Errorf("%+v, %v", cfg, err)
	}
	cfg, err = parseRun(nil, &bytes.Buffer{})
	if err != nil || cfg.Concurrency != 4 || cfg.Lease != 30*time.Second {
		t.Errorf("defaults: %+v, %v", cfg, err)
	}
}

// Every kind the binary can run has a handler, and the periodic schedule only names
// kinds that have one (worker.Run refuses otherwise).
func TestHandlersCoverTheSchedule(t *testing.T) {
	h := handlers(nil)
	if _, ok := h[worker.CleanupSchedule.Kind]; !ok {
		t.Errorf("no handler for the scheduled kind %q", worker.CleanupSchedule.Kind)
	}
}
