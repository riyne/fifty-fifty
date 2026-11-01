// Command api is the Fifty Fifty HTTP server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/time/rate"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/balances"
	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/expenses"
	"github.com/riyne/fifty-fifty/internal/groups"
	"github.com/riyne/fifty-fifty/internal/httpapi"
	"github.com/riyne/fifty-fifty/internal/ratelimit"
	"github.com/riyne/fifty-fifty/internal/settlements"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	issuer := os.Getenv("OIDC_ISSUER")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	if issuer == "" || clientID == "" {
		return errors.New("OIDC_ISSUER and OIDC_CLIENT_ID are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// pgxpool.New does not dial. The server starts even if Postgres is down,
	// and /readyz reports the state.
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}
	defer pool.Close()

	// OIDC discovery happens once at startup, so a wrong issuer fails loudly
	// here instead of as 401s later. OIDC_DISCOVERY_URL is only for when the api
	// reaches the provider at a different address than the one in token claims.
	discoverCtx, cancelDiscover := context.WithTimeout(ctx, 10*time.Second)
	verifier, err := auth.NewOIDCVerifier(discoverCtx, issuer, os.Getenv("OIDC_DISCOVERY_URL"), clientID)
	cancelDiscover()
	if err != nil {
		return fmt.Errorf("init oidc: %w", err)
	}

	// Join attempts: a burst of 10, then one every 6 seconds, per user. In memory,
	// so per api process (see internal/ratelimit).
	joinLimiter := ratelimit.New(rate.Every(6*time.Second), 10)

	v1 := httpapi.NewV1(auth.Middleware(verifier, db.New(pool)), groups.NewHandler(pool, joinLimiter), expenses.NewHandler(pool), balances.NewHandler(pool), settlements.NewHandler(pool))

	// Payment webhooks are accepted only when a signing secret is configured; without
	// one the route does not exist (404), never "accept anything".
	var webhooks http.Handler
	if secret := os.Getenv("PAYMENTS_WEBHOOK_SECRET"); secret != "" {
		wh, err := settlements.NewWebhookHandler(pool, []byte(secret))
		if err != nil {
			return fmt.Errorf("payment webhooks: %w", err)
		}
		webhooks = wh
		slog.Info("payment webhooks enabled", "path", settlements.WebhookPath)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(pool, v1, webhooks),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
