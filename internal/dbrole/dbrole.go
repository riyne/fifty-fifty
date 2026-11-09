// Package dbrole derives connection settings for the database roles other than the
// application role, for tests, rigs and benchmarks that start the api's second pool
// or a worker against a database whose URL they were given for fifty_app. The
// production binaries do NOT use it: cmd/api and cmd/worker take their URLs from the
// environment, so a deployment chooses its own credentials.
//
// The password is the development throwaway set by migrations 00004 and 00015.
package dbrole

import (
	"context"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The roles, from migrations 00004 and 00015.
const (
	App    = "fifty_app"    // the user-request path (row-level security applies)
	Worker = "fifty_worker" // worker processes
	System = "fifty_system" // the api's second pool: user provisioning and the payment webhook
)

const devPassword = "fifty"

// URL returns rawURL with its user and password replaced by role's.
func URL(rawURL, role string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(role, devPassword)
	return u.String(), nil
}

// Pool opens a pool on the same database as base, connecting as role, with at most
// maxConns connections (0 keeps base's limit). The shared test database allows 100
// connections in all, so a rig that opens a second pool per api says how few it needs.
// The caller closes it.
func Pool(ctx context.Context, base *pgxpool.Pool, role string, maxConns int32) (*pgxpool.Pool, error) {
	cfg := base.Config().Copy()
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.ConnConfig.User = role
	cfg.ConnConfig.Password = devPassword
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool as %s: %w", role, err)
	}
	return p, nil
}
