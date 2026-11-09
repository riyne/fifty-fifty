// Package testutil holds helpers shared by integration tests.
package testutil

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/harness/dbrole"
)

// UniqueSubject returns an OIDC subject that no other test run has used.
func UniqueSubject(prefix string) string { return prefix + "-" + uuid.NewString() }

// Pool returns a pool on the integration-test database (TEST_DATABASE_URL),
// which must already be migrated: make up && make migrate-up.
//
// With TEST_DATABASE_URL unset the test is skipped, so plain `go test` works on
// a machine without Postgres. CI sets REQUIRE_DATABASE=1, which turns that skip
// into a failure: a CI run in which the database tests silently did not run would
// be green and prove nothing.
//
// Tests share one database and must not assume it is empty: create their own
// rows under unique identities (see UniqueSubject) rather than truncating tables.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		skipOrFail(t, "TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping test database (is it up and migrated?): %v", err)
	}
	return pool
}

// OwnerPool returns a pool connected as the database owner (TEST_OWNER_DATABASE_URL).
// Use it only for tests that must show a protection holds even against the owner
// or a superuser; everything else runs as the unprivileged app role via Pool.
// Skipped when the variable is unset.
func OwnerPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_OWNER_DATABASE_URL")
	if url == "" {
		skipOrFail(t, "TEST_OWNER_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect to owner test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping owner test database: %v", err)
	}
	return pool
}

// WorkerPool returns a pool connected as fifty_worker, the role of the worker processes
// (migration 00015), on the same database as Pool. Code that runs in the worker (the job
// handlers, the sweeps, the idempotency cleanup) is tested with it, so the test shows
// the role's grants are enough and, once row-level security applies, that its policies
// let the worker see what it must.
func WorkerPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		skipOrFail(t, "TEST_DATABASE_URL not set")
	}
	wurl, err := dbrole.URL(url, dbrole.Worker)
	if err != nil {
		t.Fatalf("worker url: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), wurl)
	if err != nil {
		t.Fatalf("connect as the worker role: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping as the worker role (is the database migrated to 00015?): %v", err)
	}
	return pool
}

var (
	sharedOwnerMu sync.Mutex
	sharedOwner   = map[string]*pgxpool.Pool{}
)

// SharedOwnerPool is OwnerPool for test helpers that start many things in one process
// (every api stack): one pool per URL for the whole process, never closed (the process
// exit does it), so that a test with a dozen stacks does not hold a dozen pools of the
// shared database's 100 connections. Tests seed and inspect state with the owner role
// because it sees every row whatever policies apply to the roles under test.
func SharedOwnerPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_OWNER_DATABASE_URL")
	if url == "" {
		skipOrFail(t, "TEST_OWNER_DATABASE_URL not set")
	}
	sharedOwnerMu.Lock()
	defer sharedOwnerMu.Unlock()
	if p, ok := sharedOwner[url]; ok {
		return p
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse owner url: %v", err)
	}
	cfg.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect to owner test database: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping owner test database: %v", err)
	}
	sharedOwner[url] = pool
	return pool
}

// skipOrFail skips the test, or fails it when REQUIRE_DATABASE=1 (set by CI).
func skipOrFail(t testing.TB, why string) {
	t.Helper()
	if os.Getenv("REQUIRE_DATABASE") == "1" {
		t.Fatalf("%s, and REQUIRE_DATABASE=1: the database tests must run, not skip", why)
	}
	t.Skipf("%s; skipping integration test", why)
}
