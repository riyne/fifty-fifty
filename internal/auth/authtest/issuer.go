// Package authtest provides an in-process OIDC issuer for tests.
//
// It is a real issuer: it serves a discovery document and a JWKS and signs
// RS256 tokens, so the production verifier (go-oidc) runs unmodified against
// it. Nothing here bypasses verification, and no production package imports it.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// T is the part of testing.TB the issuer needs. testing.TB satisfies it, so tests pass
// their *testing.T as before; a program that is not a test (the stress harness) passes
// a small implementation of its own, because it cannot get a testing.TB.
type T interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Issuer is a running fake identity provider.
type Issuer struct {
	// URL is the address it is served at: the base URL of the discovery document and the keys.
	URL string
	// Name is the issuer identifier: what its discovery document announces and what its tokens
	// carry in `iss`. NewIssuer makes it equal to URL; NewIssuerNamed makes it different, as it
	// is when the api reaches the provider at another address than the one in its tokens.
	Name string

	key *rsa.PrivateKey
	kid string
}

// NewIssuer starts a fake issuer with a fresh signing key. It is shut down
// when the test ends. Its Name is its URL.
func NewIssuer(t T) *Issuer {
	t.Helper()
	return newIssuer(t, "")
}

// NewIssuerNamed starts a fake issuer whose identity (its discovery document's `issuer` and
// the `iss` of its tokens) is name, while it is served at URL. For the case where discovery
// happens at one address and tokens name another (OIDC_DISCOVERY_URL).
func NewIssuerNamed(t T, name string) *Issuer {
	t.Helper()
	return newIssuer(t, name)
}

func newIssuer(t T, name string) *Issuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	iss := &Issuer{key: key, kid: "test-key-1"}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	iss.Name = name
	if iss.Name == "" {
		iss.Name = iss.URL
	}

	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                iss.Name,
			"authorization_endpoint":                iss.URL + "/auth",
			"token_endpoint":                        iss.URL + "/token",
			"jwks_uri":                              iss.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: iss.kid, Algorithm: string(jose.RS256), Use: "sig",
		}}})
	})
	return iss
}

// Token describes a token to mint. Zero fields get valid defaults, so a test
// overrides only the property it wants to break.
type Token struct {
	Issuer   string        // default: the issuer's own Name
	Audience string        // default: "test-client"
	Subject  string        // default: "test-subject"
	Email    string        // default: none
	Name     string        // default: none
	TTL      time.Duration // default: 5 minutes; use a negative value for an expired token
}

// Mint signs a token with this issuer's key.
func (i *Issuer) Mint(t T, tok Token) string {
	t.Helper()
	if tok.Issuer == "" {
		tok.Issuer = i.Name
	}
	if tok.Audience == "" {
		tok.Audience = "test-client"
	}
	if tok.Subject == "" {
		tok.Subject = "test-subject"
	}
	if tok.TTL == 0 {
		tok.TTL = 5 * time.Minute
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: i.key, KeyID: i.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	now := time.Now()
	std := jwt.Claims{
		Issuer:   tok.Issuer,
		Subject:  tok.Subject,
		Audience: jwt.Audience{tok.Audience},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(tok.TTL)),
	}
	extra := map[string]any{}
	if tok.Email != "" {
		extra["email"] = tok.Email
	}
	if tok.Name != "" {
		extra["name"] = tok.Name
	}
	raw, err := jwt.Signed(signer).Claims(std).Claims(extra).Serialize()
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
