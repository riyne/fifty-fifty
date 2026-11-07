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
	"strconv"
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
	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/ratelimit"
	"github.com/riyne/fifty-fifty/internal/receiptapi"
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
	// The second pool is for what runs without a user: first-login provisioning in
	// the auth middleware and the payment webhook. It must be a different role from
	// DATABASE_URL (fifty_system, migration 00015); there is no fallback to the
	// application pool, because row-level security would then hide those rows.
	systemURL := os.Getenv("SYSTEM_DATABASE_URL")
	if systemURL == "" {
		return errors.New("SYSTEM_DATABASE_URL is required (the fifty_system role)")
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
	systemPool, err := pgxpool.New(ctx, systemURL)
	if err != nil {
		return fmt.Errorf("create system pool: %w", err)
	}
	defer systemPool.Close()

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

	// Receipt upload is mounted only when RECEIPT_DIR names a directory for the images
	// (created with mode 0700 if missing; the worker that parses them must see the same
	// directory). Uploads: a burst of 5, then one every 10 seconds, per user; the daily
	// quota (RECEIPT_DAILY_QUOTA, default 20) is the cost cap.
	var receiptHandler *receiptapi.Handler
	if dir := os.Getenv("RECEIPT_DIR"); dir != "" {
		store, err := objstore.NewFS(dir)
		if err != nil {
			return fmt.Errorf("receipt store: %w", err)
		}
		quota := receiptapi.DefaultDailyQuota
		if v := os.Getenv("RECEIPT_DAILY_QUOTA"); v != "" {
			if quota, err = strconv.Atoi(v); err != nil || quota < 1 || quota > 10000 {
				return errors.New("RECEIPT_DAILY_QUOTA must be an integer from 1 to 10000")
			}
		}
		receiptHandler = receiptapi.NewHandler(pool, store, ratelimit.New(rate.Every(10*time.Second), 5), receiptapi.Config{DailyQuota: quota})
		slog.Info("receipt upload enabled", "dir", dir, "daily_quota", quota)
	}

	v1 := httpapi.NewV1(auth.Middleware(verifier, db.New(systemPool)), groups.NewHandler(pool, joinLimiter), expenses.NewHandler(pool), balances.NewHandler(pool), settlements.NewHandler(pool), receiptHandler)

	// Payment webhooks are accepted only when a signing secret is configured; without
	// one the route does not exist (404), never "accept anything".
	var webhooks http.Handler
	if secret := os.Getenv("PAYMENTS_WEBHOOK_SECRET"); secret != "" {
		wh, err := settlements.NewWebhookHandler(systemPool, []byte(secret))
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
