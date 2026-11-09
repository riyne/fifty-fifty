// Package httpx holds small JSON request/response helpers shared by handlers.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
)

// MaxBodyBytes caps request bodies.
const MaxBodyBytes = 1 << 20

// ErrEmptyBody is returned by DecodeJSON for a zero-length body, so handlers
// with an optional body can tell it apart from malformed JSON.
var ErrEmptyBody = errors.New("request body is empty")

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "err", err)
	}
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError writes the standard error body: {"code": "...", "message": "..."}.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, errorBody{Code: code, Message: message})
}

// DecodeJSON reads a single JSON object into dst. It rejects unknown fields,
// trailing data, and bodies over MaxBodyBytes. The returned error message is
// safe to show to the client.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			return fmt.Errorf("request body too large")
		case errors.Is(err, io.EOF):
			return ErrEmptyBody
		}
		return fmt.Errorf("invalid JSON body")
	}
	if dec.More() {
		return fmt.Errorf("invalid JSON body")
	}
	return nil
}

// lockNotAvailable is Postgres's SQLSTATE for a lock wait that ran past lock_timeout.
const lockNotAvailable = "55P03"

// IsLockTimeout reports whether err is a lock wait that Postgres cancelled because it
// ran past lock_timeout (dbscope sets it for every request transaction).
func IsLockTimeout(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == lockNotAvailable
}

// InternalError answers a request that failed on the server side and logs why. A lock
// wait that timed out is not a bug: it is 503 `busy` with a Retry-After, and the client
// should retry (with the same Idempotency-Key: the failed attempt rolled back, and
// nothing is stored under a key for a 5xx). Everything else is a 500 `internal` with no
// detail for the client.
func InternalError(w http.ResponseWriter, what string, err error) {
	if IsLockTimeout(err) {
		slog.Warn(what+": gave up waiting for a lock", "err", err)
		w.Header().Set("Retry-After", "1")
		WriteError(w, http.StatusServiceUnavailable, "busy", "the request waited too long for another request to finish and was not done; nothing changed, so retry it (with the same Idempotency-Key if it has one)")
		return
	}
	slog.Error(what, "err", err)
	WriteError(w, http.StatusInternalServerError, "internal", "internal error")
}
