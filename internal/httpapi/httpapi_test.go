package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/riyne/fifty-fifty/internal/httpapi"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

// newHealthOnly builds the handler without the /v1 API (nil v1 mounts nothing).
func newHealthOnly(db httpapi.Pinger) http.Handler { return httpapi.New(db, nil, nil) }

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHealthz_IgnoresDatabase(t *testing.T) {
	h := newHealthOnly(fakeDB{err: errors.New("db down")})
	if rec := get(t, h, "GET", "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 even when the db is down", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"db up", nil, http.StatusOK},
		{"db down", errors.New("connection refused"), http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, newHealthOnly(fakeDB{err: tt.err}), "GET", "/readyz")
			if rec.Code != tt.want {
				t.Fatalf("readyz = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	rec := get(t, newHealthOnly(fakeDB{}), "POST", "/healthz")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz = %d, want 405", rec.Code)
	}
}
