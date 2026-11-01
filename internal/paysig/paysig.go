// Package paysig signs and verifies payment-provider webhooks. The mock provider
// signs with it and the api's webhook endpoint verifies with it, so both ends agree
// by construction.
//
// The scheme is the usual one: HMAC-SHA256 over "<unix seconds>.<body>" with a shared
// secret, sent as the header value "t=<unix seconds>,v1=<hex>". The timestamp is part
// of what is signed, so a captured request cannot be replayed with a fresh timestamp,
// and Verify rejects a timestamp outside a tolerance window, which bounds how long a
// captured request stays replayable. (Within the window a replay still verifies: the
// receiver's handling must be idempotent, and settlement resolution is.)
package paysig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Header is the HTTP header that carries the signature.
const Header = "X-Payments-Signature"

var (
	// ErrMalformed means the header is not "t=<seconds>,v1=<hex>[,v1=<hex>...]".
	ErrMalformed = errors.New("paysig: malformed signature header")
	// ErrSignature means no v1 signature matches the body and timestamp.
	ErrSignature = errors.New("paysig: signature does not match")
	// ErrExpired means the timestamp is outside the tolerance window.
	ErrExpired = errors.New("paysig: timestamp outside the tolerance window")
	// ErrNoSecret means the secret is empty; an empty key would make the signature
	// forgeable by anyone.
	ErrNoSecret = errors.New("paysig: empty secret")
)

func mac(secret []byte, unix int64, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(unix, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return m.Sum(nil)
}

// Sign returns the header value for body, signed at time t.
func Sign(secret []byte, t time.Time, body []byte) (string, error) {
	if len(secret) == 0 {
		return "", ErrNoSecret
	}
	return "t=" + strconv.FormatInt(t.Unix(), 10) + ",v1=" + hex.EncodeToString(mac(secret, t.Unix(), body)), nil
}

// Verify checks header against body. now is passed in so tests do not depend on the
// clock. A header may carry several v1 values (a secret being rotated); any one that
// matches is enough. The comparison is constant-time.
func Verify(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) error {
	if len(secret) == 0 {
		return ErrNoSecret
	}
	var (
		unix    int64
		haveT   bool
		sigs    [][]byte
		partsOK = true
	)
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			partsOK = false
			break
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || haveT {
				partsOK = false
			}
			unix, haveT = n, true
		case "v1":
			b, err := hex.DecodeString(v)
			if err != nil || len(b) != sha256.Size {
				partsOK = false
			}
			sigs = append(sigs, b)
		default:
			// Unknown keys are ignored so the scheme can grow (a v2) without
			// breaking old receivers.
		}
	}
	if !partsOK || !haveT || len(sigs) == 0 {
		return ErrMalformed
	}

	// Check the signature before the clock: an expired header with a bad signature is
	// a forgery, not an old request, and should not look like one.
	want := mac(secret, unix, body)
	matched := false
	for _, s := range sigs {
		if hmac.Equal(s, want) { // no early exit: every candidate is compared
			matched = true
		}
	}
	if !matched {
		return ErrSignature
	}
	skew := now.Sub(time.Unix(unix, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > tolerance {
		return ErrExpired
	}
	return nil
}
