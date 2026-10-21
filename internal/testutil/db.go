// Package testutil holds helpers shared by integration tests.
package testutil

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UniqueSubject returns an OIDC subject that no other test run has used.
func UniqueSubject(prefix string) string { return prefix + "-" + uuid.NewString() }

// Pool returns a pool on the integration-test database (TEST_DATABASE_URL),
// which must already be migrated: make up && make migrate-up.
//
// With TEST_DATABASE_URL unset the test is skipped, so plain `go test` works on
// a machine without Postgres. CI must set it (and should fail on skips).
//
// Tests share one database and must not assume it is empty: create their own
// rows under unique identities (see UniqueSubject) rather than truncating tables.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
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
