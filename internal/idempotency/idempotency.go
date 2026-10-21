// Package idempotency makes a state-changing request safe to retry.
//
// A client sends an Idempotency-Key header. The first request with a key runs;
// any later request with the same key and the same fingerprint gets the first
// request's stored response instead of running again. The same key with a
// different fingerprint is a client bug and is rejected (ErrKeyReuse).
//
// How it works:
//
//  1. Do opens a transaction and claims the key by inserting a placeholder row.
//  2. If a live row already exists, Do replays it (or rejects a fingerprint
//     mismatch) and the transaction rolls back untouched.
//  3. If another transaction is mid-flight on the same key, the insert blocks on
//     the primary-key index until that transaction commits or rolls back, then
//     sees its result. So concurrent duplicates serialize and the second one
//     replays; the operation never runs twice.
//  4. Otherwise fn runs in the same transaction. Its writes, the stored response
//     and the key row all commit together or not at all. There is no
//     "in progress" state to get stuck: a crash rolls the claim back too.
//
// What is stored: every response with status < 500 that fn returns, including
// business-rule failures such as 404, so a retry sees the original outcome (the
// Stripe model). 5xx results and errors roll everything back and are not
// stored, because the transaction is atomic: a 5xx never leaves partial
// effects, so a retry should be allowed to succeed. Validation errors, 401 and
// 429 are rejected before Do is called and never claim a key.
//
// Contract for fn: when it returns a non-2xx result it must not have written
// anything it does not want committed, because that result is committed.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/db"
)

// ErrKeyReuse means the key was already used for a different request.
var ErrKeyReuse = errors.New("idempotency key reused with a different request")

// ErrInvalidKey means the header is missing or malformed.
var ErrInvalidKey = errors.New("Idempotency-Key header is required (1-255 visible ASCII characters)")

// ParseKey reads and validates the Idempotency-Key header.
func ParseKey(r *http.Request) (string, error) {
	k := r.Header.Get("Idempotency-Key")
	if k == "" || len(k) > 255 {
		return "", ErrInvalidKey
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return "", ErrInvalidKey
		}
	}
	return k, nil
}

// Fingerprint identifies "the same request": hash the parts that define it
// (method, route, and the meaningful body fields). Parts are length-prefixed so
// ("ab","c") and ("a","bc") differ.
func Fingerprint(parts ...string) []byte {
	h := sha256.New()
	for _, p := range parts {
		_ = binary.Write(h, binary.BigEndian, uint32(len(p)))
		h.Write([]byte(p))
	}
	return h.Sum(nil)
}

// Result is a complete HTTP response to store and replay. Body is JSON.
type Result struct {
	Status int
	Body   []byte
}

// Outcome is what Do hands back to the handler.
type Outcome struct {
	Result
	Replayed bool // true if this is a stored response from an earlier request
}

// Do runs fn at most once per (userID, key). See the package comment.
func Do(
	ctx context.Context,
	pool *pgxpool.Pool,
	userID uuid.UUID,
	key string,
	fingerprint []byte,
	fn func(ctx context.Context, tx pgx.Tx) (Result, error),
) (Outcome, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	q := db.New(tx)

	claimed := false
	// The loop only repeats if a row we conflicted with disappears before we can
	// read it (expiry cleanup). Two passes is more than enough.
	for range 2 {
		_, err := q.ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{UserID: userID, Key: key, Fingerprint: fingerprint})
		if err == nil {
			claimed = true
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Outcome{}, fmt.Errorf("claim key: %w", err)
		}
		prev, err := q.GetLiveIdempotencyKey(ctx, db.GetLiveIdempotencyKeyParams{UserID: userID, Key: key})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return Outcome{}, fmt.Errorf("read key: %w", err)
		}
		if !bytes.Equal(prev.Fingerprint, fingerprint) {
			return Outcome{}, ErrKeyReuse
		}
		return Outcome{Result: Result{Status: int(prev.ResponseStatus), Body: prev.ResponseBody}, Replayed: true}, nil
	}
	if !claimed {
		return Outcome{}, errors.New("could not claim idempotency key")
	}

	res, err := fn(ctx, tx)
	if err != nil {
		return Outcome{}, err // deferred rollback discards the claim and fn's writes
	}
	if res.Status >= 500 {
		return Outcome{Result: res}, nil // rolled back by the defer; not stored
	}
	err = q.CompleteIdempotencyKey(ctx, db.CompleteIdempotencyKeyParams{
		UserID: userID, Key: key, ResponseStatus: int32(res.Status), ResponseBody: res.Body,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("store response: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Outcome{}, fmt.Errorf("commit: %w", err)
	}
	return Outcome{Result: res}, nil
}
