package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/httpx"
)

type ctxKey struct{}

// UserFrom returns the authenticated user set by Middleware.
func UserFrom(ctx context.Context) (db.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(db.User)
	return u, ok
}

// Middleware authenticates every request: it requires a valid bearer token,
// provisions the local user on first sight, and stores the user in the
// request context. There is deliberately no way to turn it off.
func Middleware(v Verifier, q *db.Queries) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				unauthorized(w)
				return
			}
			claims, err := v.Verify(r.Context(), raw)
			if err != nil {
				// Never log the token. The error describes why verification
				// failed (expired, wrong audience, ...) which is useful and safe.
				slog.Info("token rejected", "err", err)
				unauthorized(w)
				return
			}
			user, err := resolveUser(r.Context(), q, claims)
			if err != nil {
				slog.Error("resolve user", "err", err)
				httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer`)
	httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
}

// resolveUser finds the user for (issuer, subject), creating it on first login.
// The common path is a single read; the insert only runs for new users and is
// safe under concurrent first requests (see EnsureUser).
func resolveUser(ctx context.Context, q *db.Queries, c Claims) (db.User, error) {
	u, err := q.GetUserByIdentity(ctx, db.GetUserByIdentityParams{Issuer: c.Issuer, Subject: c.Subject})
	if err == nil {
		return u, nil
	}
	if err != pgx.ErrNoRows {
		return db.User{}, err
	}
	return q.EnsureUser(ctx, db.EnsureUserParams{
		Issuer:      c.Issuer,
		Subject:     c.Subject,
		Email:       truncate(c.Email, 254),
		DisplayName: truncate(c.Name, 100),
	})
}

// truncate bounds attacker-influenced display data to n runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
