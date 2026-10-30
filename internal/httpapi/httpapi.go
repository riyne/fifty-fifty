// Package httpapi builds the HTTP handler for the api binary.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/expenses"
	"github.com/riyne/fifty-fifty/internal/groups"
	"github.com/riyne/fifty-fifty/internal/httpx"
)

// Pinger is the part of the database pool that the readiness check needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

const readyTimeout = 2 * time.Second

// New returns the root handler. Health routes are unauthenticated. v1 serves
// everything under /v1/ and must already be wrapped in authentication (see
// NewV1); a nil v1 mounts nothing, so a missing wiring fails closed with 404s.
// Routes use Go 1.22+ method-aware patterns.
func New(db Pinger, v1 http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz(db))
	if v1 != nil {
		mux.Handle("/v1/", v1)
	}
	return mux
}

// NewV1 builds the authenticated API. authn wraps the whole mux, so a route
// added here cannot accidentally be served without authentication.
func NewV1(authn func(http.Handler) http.Handler, g *groups.Handler, e *expenses.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/me", me)
	g.Register(mux)
	e.Register(mux)
	return authn(mux)
}

// healthz is liveness: the process is up and serving. It must not touch
// dependencies, otherwise a database outage would make an orchestrator
// restart a perfectly healthy api.
func healthz(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is readiness: the api can reach Postgres right now.
func readyz(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			slog.Warn("readiness check failed", "err", err)
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "db": "down"})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "db": "up"})
	}
}

func me(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id":           u.ID,
		"email":        u.Email,
		"display_name": u.DisplayName,
	})
}
