package paysig_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/paysig"
)

var (
	secret = []byte("whsec_test")
	now    = time.Unix(1_800_000_000, 0)
	body   = []byte(`{"id":"evt_1","type":"payment.succeeded"}`)
	tol    = 5 * time.Minute
)

func sign(t *testing.T, at time.Time) string {
	t.Helper()
	h, err := paysig.Sign(secret, at, body)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRoundTrip(t *testing.T) {
	if err := paysig.Verify(secret, sign(t, now), body, now, tol); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRefuses(t *testing.T) {
	good := sign(t, now)
	otherSecret, _ := paysig.Sign([]byte("other"), now, body)
	laterBody := []byte(`{"id":"evt_1","type":"payment.failed"}`)

	tests := []struct {
		name   string
		secret []byte
		header string
		body   []byte
		at     time.Time
		want   error
	}{
		{"tampered body", secret, good, laterBody, now, paysig.ErrSignature},
		{"wrong secret", secret, otherSecret, body, now, paysig.ErrSignature},
		{"timestamp changed to a fresh one", secret, strings.Replace(good, "t=1800000000", "t=1800000100", 1), body, now, paysig.ErrSignature},
		{"too old", secret, sign(t, now.Add(-tol-time.Second)), body, now, paysig.ErrExpired},
		{"too far in the future", secret, sign(t, now.Add(tol+time.Second)), body, now, paysig.ErrExpired},
		{"empty header", secret, "", body, now, paysig.ErrMalformed},
		{"no timestamp", secret, "v1=" + good[strings.Index(good, "v1=")+3:], body, now, paysig.ErrMalformed},
		{"no signature", secret, "t=1800000000", body, now, paysig.ErrMalformed},
		{"timestamp not a number", secret, "t=soon,v1=" + good[strings.Index(good, "v1=")+3:], body, now, paysig.ErrMalformed},
		{"signature not hex", secret, "t=1800000000,v1=zz", body, now, paysig.ErrMalformed},
		{"signature wrong length", secret, "t=1800000000,v1=abcd", body, now, paysig.ErrMalformed},
		{"two timestamps", secret, "t=1800000000," + good, body, now, paysig.ErrMalformed},
		{"a part without =", secret, good + ",garbage", body, now, paysig.ErrMalformed},
		{"empty secret", nil, good, body, now, paysig.ErrNoSecret},
	}
	for _, tc := range tests {
		if err := paysig.Verify(tc.secret, tc.header, tc.body, tc.at, tol); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// A forged header that is also stale must be reported as a bad signature, not as an
// old request: "expired" would tell an attacker their signature was otherwise right.
func TestBadSignatureIsReportedBeforeExpiry(t *testing.T) {
	forged := "t=1,v1=" + strings.Repeat("00", 32)
	if err := paysig.Verify(secret, forged, body, now, tol); !errors.Is(err, paysig.ErrSignature) {
		t.Errorf("err = %v, want ErrSignature", err)
	}
}

func TestSeveralSignaturesAndUnknownKeys(t *testing.T) {
	good := sign(t, now)
	stale, _ := paysig.Sign([]byte("old secret"), now, body)
	v1 := good[strings.Index(good, "v1="):]

	// A secret being rotated: the header carries a signature for each; either matches.
	rotated := stale + "," + v1
	if err := paysig.Verify(secret, rotated, body, now, tol); err != nil {
		t.Errorf("rotated header: %v", err)
	}
	// Unknown keys are skipped so a later scheme version does not break this one.
	if err := paysig.Verify(secret, good+",v2=whatever", body, now, tol); err != nil {
		t.Errorf("header with v2: %v", err)
	}
	// Spaces around parts are tolerated.
	if err := paysig.Verify(secret, strings.ReplaceAll(good, ",", ", "), body, now, tol); err != nil {
		t.Errorf("header with spaces: %v", err)
	}
}

func TestSignRefusesAnEmptySecret(t *testing.T) {
	if _, err := paysig.Sign(nil, now, body); !errors.Is(err, paysig.ErrNoSecret) {
		t.Errorf("err = %v, want ErrNoSecret", err)
	}
}
