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

// ---- OIDC_DISCOVERY_URL: discovery at one address, tokens naming another ----
//
// The api in a container reaches the identity provider at http://dex:5556 while the tokens (and
// the provider's own discovery document) say http://localhost:5556. The fake provider here is
// named "https://login.example.test", which nothing resolves, and is served at a local address.

const tokenIssuer = "https://login.example.test"

func TestNewOIDCVerifier_DiscoveryURLSplit(t *testing.T) {
	iss := authtest.NewIssuerNamed(t, tokenIssuer)
	other := authtest.NewIssuerNamed(t, tokenIssuer) // same name, a different signing key
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := auth.NewOIDCVerifier(ctx, iss.Name, iss.URL, clientID)
	if err != nil {
		t.Fatalf("split discovery: %v", err)
	}
	got, err := v.Verify(ctx, iss.Mint(t, authtest.Token{Subject: "s1"}))
	if err != nil || got.Issuer != tokenIssuer || got.Subject != "s1" {
		t.Fatalf("a token naming the configured issuer: %+v, %v, want issuer %q", got, err, tokenIssuer)
	}

	// The keys come from where discovery was done, and the issuer is checked against the
	// configured name, not against the address the keys were fetched from.
	for name, token := range map[string]string{
		"a token naming the discovery address instead of the issuer": iss.Mint(t, authtest.Token{Issuer: iss.URL}),
		"a token naming some other issuer":                           iss.Mint(t, authtest.Token{Issuer: "https://evil.example"}),
		"a token signed by a key the provider did not publish":       other.Mint(t, authtest.Token{}),
		"an expired token":   iss.Mint(t, authtest.Token{TTL: -time.Hour}),
		"the wrong audience": iss.Mint(t, authtest.Token{Audience: "some-other-client"}),
	} {
		if _, err := v.Verify(ctx, token); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Without a separate discovery URL the issuer name is also the address, so a provider that
// is only reachable somewhere else cannot be found: the split is what makes it work.
func TestNewOIDCVerifier_NoSplitNeedsTheIssuerToBeReachable(t *testing.T) {
	iss := authtest.NewIssuerNamed(t, tokenIssuer)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := auth.NewOIDCVerifier(ctx, iss.Name, "", clientID); err == nil {
		t.Fatal("discovery at an unreachable issuer succeeded")
	}
}

// A discovery URL equal to the issuer is the same as none.
func TestNewOIDCVerifier_DiscoveryURLEqualToIssuerIsNotASplit(t *testing.T) {
	iss := authtest.NewIssuer(t)
	v, err := auth.NewOIDCVerifier(context.Background(), iss.URL, iss.URL, clientID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), iss.Mint(t, authtest.Token{})); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A misconfiguration fails at startup, loudly: the provider found at the discovery URL announces
// an issuer other than the one the api was told to expect, so no token could ever be valid.
func TestNewOIDCVerifier_SplitWhoseProviderAnnouncesAnotherIssuerIsRefused(t *testing.T) {
	iss := authtest.NewIssuerNamed(t, "https://some-other-provider.example.test")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := auth.NewOIDCVerifier(ctx, tokenIssuer, iss.URL, clientID); err == nil {
		t.Fatal("a provider announcing a different issuer than the configured one was accepted")
	}
}
