package auth_test

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/auth"
	"github.com/riyne/fifty-fifty/internal/auth/authtest"
)

const clientID = "test-client"

func newVerifier(t *testing.T, iss *authtest.Issuer) auth.Verifier {
	t.Helper()
	v, err := auth.NewOIDCVerifier(context.Background(), iss.URL, "", clientID)
	if err != nil {
		t.Fatalf("NewOIDCVerifier: %v", err)
	}
	return v
}

func TestVerify_ValidToken(t *testing.T) {
	iss := authtest.NewIssuer(t)
	v := newVerifier(t, iss)

	got, err := v.Verify(context.Background(), iss.Mint(t, authtest.Token{
		Subject: "sub-1", Email: "a@example.com", Name: "Alice",
	}))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := auth.Claims{Issuer: iss.URL, Subject: "sub-1", Email: "a@example.com", Name: "Alice"}
	if got != want {
		t.Fatalf("claims = %+v, want %+v", got, want)
	}
}

func TestVerify_Rejects(t *testing.T) {
	iss := authtest.NewIssuer(t)
	other := authtest.NewIssuer(t) // a different signing key, also announcing key id "test-key-1"
	v := newVerifier(t, iss)

	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+iss.URL+`","sub":"x","aud":"`+clientID+`","exp":9999999999}`)) + "."

	tests := []struct {
		name  string
		token string
	}{
		{"expired", iss.Mint(t, authtest.Token{TTL: -time.Hour})},
		{"wrong audience", iss.Mint(t, authtest.Token{Audience: "some-other-client"})},
		{"wrong issuer claim", iss.Mint(t, authtest.Token{Issuer: "https://evil.example"})},
		// Right issuer claim and key id, but signed with a key the issuer never published.
		{"forged signature", other.Mint(t, authtest.Token{Issuer: iss.URL})},
		{"alg none", none},
		{"garbage", "not-a-jwt"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), tt.token); err == nil {
				t.Fatal("Verify succeeded, want error")
			}
		})
	}
}

func TestNewOIDCVerifier_RequiresConfig(t *testing.T) {
	if _, err := auth.NewOIDCVerifier(context.Background(), "", "", clientID); err == nil {
		t.Error("empty issuer accepted")
	}
	if _, err := auth.NewOIDCVerifier(context.Background(), "http://x", "", ""); err == nil {
		t.Error("empty client id accepted")
	}
}
