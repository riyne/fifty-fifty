package httpx_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/riyne/fifty-fifty/internal/httpx"
)

// A lock wait that timed out is the caller's cue to retry, not a server bug; every other
// failure stays a 500 that tells the client nothing.
func TestInternalError_LockTimeoutIsBusyAndEverythingElseIs500(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{"lock timeout", &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}, 503, "busy", "1"},
		{"lock timeout, wrapped as callers wrap", fmt.Errorf("claim key: %w", &pgconn.PgError{Code: "55P03"}), 503, "busy", "1"},
		{"deadlock is not a lock timeout", &pgconn.PgError{Code: "40P01"}, 500, "internal", ""},
		{"serialization failure is not one either", &pgconn.PgError{Code: "40001"}, 500, "internal", ""},
		{"any other error", errors.New("boom"), 500, "internal", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			httpx.InternalError(rec, "test", tc.err)
			if rec.Code != tc.status || rec.Header().Get("Retry-After") != tc.retryAfter {
				t.Fatalf("status %d Retry-After %q, want %d %q", rec.Code, rec.Header().Get("Retry-After"), tc.status, tc.retryAfter)
			}
			if !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Errorf("body %s, want code %q", rec.Body.String(), tc.code)
			}
			if tc.status == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "boom") {
				t.Errorf("a 500 leaked the error: %s", rec.Body.String())
			}
		})
	}
}
