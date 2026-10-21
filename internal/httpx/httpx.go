// Package httpx holds small JSON request/response helpers shared by handlers.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// MaxBodyBytes caps request bodies (DESIGN.md section 9: request size limits).
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
