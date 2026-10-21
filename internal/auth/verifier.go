// Package auth validates OIDC bearer tokens and maps them to local users.
package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Claims is the subset of token claims the api uses. Issuer and Subject
// together identify a user; Email and Name are display data only and are never
// used for identity or authorization.
type Claims struct {
	Issuer  string
	Subject string
	Email   string
	Name    string
}

// Verifier validates a raw bearer token. The error is for logging only and
// must not be shown to clients.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Claims, error)
}

type oidcVerifier struct {
	v *oidc.IDTokenVerifier
}

// NewOIDCVerifier discovers the provider and returns a Verifier that checks
// signature (via the provider's JWKS), issuer, audience (clientID) and expiry.
// Only RS256 is accepted (go-oidc's default).
//
// discoveryURL may differ from issuer for setups where the api reaches the
// provider at another address than the one baked into tokens (e.g. the api in
// a container reaching Dex as http://dex:5556 while tokens say
// http://localhost:5556). Leave it empty when they are the same.
func NewOIDCVerifier(ctx context.Context, issuer, discoveryURL, clientID string) (Verifier, error) {
	if issuer == "" || clientID == "" {
		return nil, errors.New("issuer and client id are required")
	}
	if discoveryURL != "" && discoveryURL != issuer {
		ctx = oidc.InsecureIssuerURLContext(ctx, issuer)
	} else {
		discoveryURL = issuer
	}
	p, err := oidc.NewProvider(ctx, discoveryURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery at %s: %w", discoveryURL, err)
	}
	return &oidcVerifier{v: p.Verifier(&oidc.Config{ClientID: clientID})}, nil
}

func (o *oidcVerifier) Verify(ctx context.Context, rawToken string) (Claims, error) {
	tok, err := o.v.Verify(ctx, rawToken)
	if err != nil {
		return Claims{}, err
	}
	var extra struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := tok.Claims(&extra); err != nil {
		return Claims{}, fmt.Errorf("decode claims: %w", err)
	}
	if tok.Subject == "" {
		return Claims{}, errors.New("token has no subject")
	}
	return Claims{Issuer: tok.Issuer, Subject: tok.Subject, Email: extra.Email, Name: extra.Name}, nil
}
