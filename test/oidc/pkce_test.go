// Package oidc_test drives the real Dex the way the mobile app's login does (Authorization
// Code with PKCE, S256, the app's client id, scopes and redirect URI) and checks what the
// flow must give and what it must refuse. Nothing here is faked: it needs the compose Dex
// (`make up`), and is skipped without it unless REQUIRE_DEX=1 (CI sets it).
//
// What it does not cover: the app's own code (the Jest tests and the by-hand runs on the iOS
// Simulator do), the system browser, and refresh tokens (the app asks for none).
package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/auth"
)

// The app's request, from mobile/src/auth/oidc.ts and deploy/dex/config.yaml.
const (
	clientID    = "fifty-fifty"
	redirectURI = "fiftyfifty://auth"
	scope       = "openid email profile offline_access" // offline_access since M10.21: Dex issues a refresh token
)

func issuer() string {
	if v := os.Getenv("OIDC_ISSUER"); v != "" {
		return v
	}
	return "http://localhost:5556/dex"
}

// requireDex skips the test when Dex is not reachable, or fails it when REQUIRE_DEX=1.
func requireDex(t *testing.T) {
	t.Helper()
	resp, err := http.Get(issuer() + "/.well-known/openid-configuration")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return
		}
	}
	if os.Getenv("REQUIRE_DEX") == "1" {
		t.Fatalf("Dex is not reachable at %s (REQUIRE_DEX=1): %v", issuer(), err)
	}
	t.Skipf("Dex is not reachable at %s (make up); skipping", issuer())
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type pkce struct{ verifier, challenge string }

func newPKCE(t *testing.T) pkce {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	v := b64(raw)
	sum := sha256.Sum256([]byte(v))
	return pkce{verifier: v, challenge: b64(sum[:])}
}

// authParams is the authorization request the app builds (expo-auth-session's AuthRequest with
// usePKCE): code flow, the three scopes, the app's redirect URI, a state, an S256 challenge.
func authParams(p pkce, state string) url.Values {
	return url.Values{
		"client_id": {clientID}, "response_type": {"code"}, "scope": {scope}, "redirect_uri": {redirectURI},
		"state": {state}, "code_challenge": {p.challenge}, "code_challenge_method": {"S256"},
	}
}

// outcome is what an authorization attempt ended in.
type outcome struct {
	status   int      // of the last response Dex gave
	redirect *url.URL // where Dex sent the browser at the end, if it did
	body     string
}

func (o outcome) code() string  { return o.redirect.Query().Get("code") }
func (o outcome) state() string { return o.redirect.Query().Get("state") }

// login does what the browser does: open the authorization URL, follow Dex's redirects to its
// login form, submit the credentials, and stop at the redirect to the app (a custom scheme the
// client cannot, and need not, follow).
func login(t *testing.T, params url.Values, user, password string) outcome {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar: jar, Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return http.ErrUseLastResponse // the app's own scheme: this is where the browser hands over
			}
			return nil
		},
	}
	resp, err := client.Get(issuer() + "/auth?" + params.Encode())
	if err != nil {
		t.Fatalf("authorization request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if loc, err := resp.Location(); err == nil && resp.StatusCode/100 == 3 {
		return outcome{status: resp.StatusCode, redirect: loc, body: string(body)} // refused already: an error redirect
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(strings.ToLower(string(body)), "<form") {
		return outcome{status: resp.StatusCode, body: string(body)} // refused, with an error page
	}
	// The login form posts to the URL it was served from.
	form := url.Values{"login": {user}, "password": {password}}
	resp, err = client.PostForm(resp.Request.URL.String(), form)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	out := outcome{status: resp.StatusCode, body: string(body)}
	if loc, err := resp.Location(); err == nil && resp.StatusCode/100 == 3 {
		out.redirect = loc
	}
	return out
}

type tokenResult struct {
	status int
	fields map[string]any
}

// tokenClient has a timeout: a Dex that stops answering (it did, in M10.21, on a refresh request)
// must fail a test, not hang the suite.
var tokenClient = &http.Client{Timeout: 15 * time.Second}

func postToken(t *testing.T, form url.Values) tokenResult {
	t.Helper()
	resp, err := tokenClient.PostForm(issuer()+"/token", form)
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer resp.Body.Close()
	var f map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		t.Fatalf("token response is not JSON (HTTP %d): %v", resp.StatusCode, err)
	}
	return tokenResult{status: resp.StatusCode, fields: f}
}

func exchange(t *testing.T, code, verifier, redirect string) tokenResult {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "client_id": {clientID}}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	return postToken(t, form)
}

func refresh(t *testing.T, refreshToken, client string) tokenResult {
	t.Helper()
	return postToken(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {client}})
}

// freshCode runs a full, correct login and returns the code and the PKCE pair that goes with it.
func freshCode(t *testing.T) (string, pkce) {
	t.Helper()
	p := newPKCE(t)
	o := login(t, authParams(p, "state-"+p.verifier[:8]), "alice@example.com", "password")
	if o.redirect == nil || o.code() == "" {
		t.Fatalf("a correct login gave no code: HTTP %d, redirect %v", o.status, o.redirect)
	}
	return o.code(), p
}

func TestDiscovery_AdvertisesOnlyS256(t *testing.T) {
	requireDex(t)
	resp, err := http.Get(issuer() + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d struct {
		Issuer  string   `json:"issuer"`
		Methods []string `json:"code_challenge_methods_supported"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.Issuer != issuer() {
		t.Errorf("issuer = %q, want %q", d.Issuer, issuer())
	}
	if len(d.Methods) != 1 || d.Methods[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v, want exactly [S256]", d.Methods)
	}
}

// The login the app does, end to end, and the check the api makes of what comes out of it.
func TestPKCE_TheAppsLoginGivesAnIDTokenTheApiAccepts(t *testing.T) {
	requireDex(t)
	p := newPKCE(t)
	o := login(t, authParams(p, "the-state"), "alice@example.com", "password")
	if o.redirect == nil || o.redirect.Scheme != "fiftyfifty" || o.redirect.Host != "auth" {
		t.Fatalf("Dex did not send the browser to the app: HTTP %d, %v", o.status, o.redirect)
	}
	if o.state() != "the-state" {
		t.Errorf("state came back as %q, want the one sent: the app's check of it would fail", o.state())
	}
	if o.code() == "" {
		t.Fatalf("no code in %v", o.redirect)
	}

	tok := exchange(t, o.code(), p.verifier, redirectURI)
	if tok.status != http.StatusOK {
		t.Fatalf("exchange with the right verifier = %d %v", tok.status, tok.fields)
	}
	idToken, _ := tok.fields["id_token"].(string)
	if idToken == "" {
		t.Fatalf("no id_token in %v", tok.fields)
	}
	// The app asks for offline_access: a refresh token comes with the ID token.
	if rt, _ := tok.fields["refresh_token"].(string); rt == "" {
		t.Errorf("no refresh token although the app asks for offline_access: %v", tok.fields)
	}

	// The api's own verifier (signature from Dex's published keys, issuer, audience, expiry).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, err := auth.NewOIDCVerifier(ctx, issuer(), "", clientID)
	if err != nil {
		t.Fatalf("NewOIDCVerifier against Dex: %v", err)
	}
	claims, err := v.Verify(ctx, idToken)
	if err != nil {
		t.Fatalf("the api's verifier rejected a token Dex just issued: %v", err)
	}
	if claims.Issuer != issuer() || claims.Subject == "" || claims.Email != "alice@example.com" {
		t.Errorf("claims = %+v, want Dex's issuer, a subject and alice's email", claims)
	}
	// A different client id must not accept it (audience).
	other, err := auth.NewOIDCVerifier(ctx, issuer(), "", "some-other-client")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Verify(ctx, idToken); err == nil {
		t.Error("a verifier for another client accepted this client's token")
	}
	// And the token is not valid for anyone else: a tampered one is refused.
	if _, err := v.Verify(ctx, idToken[:len(idToken)-3]+"AAA"); err == nil {
		t.Error("a token with a damaged signature was accepted")
	}
}

// What PKCE is for: a code that is stolen (or replayed, or redeemed wrongly) is worthless.
func TestPKCE_ACodeIsUselessWithoutTheRightVerifierAndOnlyWorksOnce(t *testing.T) {
	requireDex(t)
	refused := func(t *testing.T, r tokenResult, why string) {
		t.Helper()
		if r.status != http.StatusBadRequest || r.fields["id_token"] != nil {
			t.Errorf("%s: HTTP %d %v, want a 400 and no token", why, r.status, r.fields)
		}
	}

	t.Run("the wrong verifier", func(t *testing.T) {
		code, _ := freshCode(t)
		refused(t, exchange(t, code, newPKCE(t).verifier, redirectURI), "a verifier from another login")
	})
	t.Run("no verifier at all", func(t *testing.T) {
		code, _ := freshCode(t)
		refused(t, exchange(t, code, "", redirectURI), "a stolen code with no verifier")
	})
	t.Run("a different redirect URI at the exchange", func(t *testing.T) {
		code, p := freshCode(t)
		refused(t, exchange(t, code, p.verifier, "http://localhost:8080/callback"), "a redirect URI other than the one the code was issued for")
	})
	t.Run("the same code twice", func(t *testing.T) {
		code, p := freshCode(t)
		if first := exchange(t, code, p.verifier, redirectURI); first.status != http.StatusOK {
			t.Fatalf("the first exchange failed: %d %v", first.status, first.fields)
		}
		refused(t, exchange(t, code, p.verifier, redirectURI), "a replayed code")
	})
	t.Run("a refused attempt does not make the code usable without a verifier", func(t *testing.T) {
		// Whether Dex burns the code after a refusal or not, the thief must not get a token with it.
		code, _ := freshCode(t)
		refused(t, exchange(t, code, newPKCE(t).verifier, redirectURI), "the thief's guess")
		if again := exchange(t, code, "", redirectURI); again.status == http.StatusOK {
			t.Errorf("after a wrong attempt the code worked with no verifier: %v", again.fields)
		}
	})
}

// A client that leaves PKCE out, or asks for the weak `plain` method, is refused at the
// authorization request (deploy/dex/config.yaml: pkce.enforce and S256 only). Without that, Dex
// issued a code with no challenge at all for this public client.
func TestPKCE_NoChallengeAndPlainAreRefusedAtTheAuthorizationRequest(t *testing.T) {
	requireDex(t)
	p := newPKCE(t)
	for name, mutate := range map[string]func(url.Values){
		"no code_challenge":         func(v url.Values) { v.Del("code_challenge"); v.Del("code_challenge_method") },
		"a challenge, no method":    func(v url.Values) { v.Del("code_challenge_method") },
		"the plain method":          func(v url.Values) { v.Set("code_challenge", p.verifier); v.Set("code_challenge_method", "plain") },
		"an unknown method":         func(v url.Values) { v.Set("code_challenge_method", "S512") },
		"an empty challenge (S256)": func(v url.Values) { v.Set("code_challenge", "") },
	} {
		t.Run(name, func(t *testing.T) {
			params := authParams(p, "s")
			mutate(params)
			o := login(t, params, "alice@example.com", "password")
			if o.redirect != nil && o.code() != "" {
				t.Fatalf("Dex issued a code: %v", o.redirect)
			}
			if o.redirect == nil && o.status/100 == 2 && strings.Contains(strings.ToLower(o.body), "<form") {
				t.Fatalf("Dex showed a login form for a request without S256 PKCE")
			}
		})
	}
}

// The other things the app's login depends on Dex refusing.
func TestLogin_RefusesWhatItShould(t *testing.T) {
	requireDex(t)
	p := newPKCE(t)

	t.Run("a redirect URI that is not registered", func(t *testing.T) {
		params := authParams(p, "s")
		params.Set("redirect_uri", "https://evil.example/cb")
		o := login(t, params, "alice@example.com", "password")
		if o.status != http.StatusBadRequest || (o.redirect != nil && o.redirect.Host == "evil.example") {
			t.Errorf("HTTP %d, redirect %v: want a 400 and no redirect to the unregistered URI", o.status, o.redirect)
		}
	})
	t.Run("a client that does not exist", func(t *testing.T) {
		params := authParams(p, "s")
		params.Set("client_id", "no-such-client")
		if o := login(t, params, "alice@example.com", "password"); o.status != http.StatusBadRequest || (o.redirect != nil && o.code() != "") {
			t.Errorf("HTTP %d, redirect %v: want a 400 and no code", o.status, o.redirect)
		}
	})
	t.Run("a wrong password", func(t *testing.T) {
		o := login(t, authParams(p, "s"), "alice@example.com", "not-the-password")
		if o.redirect != nil || o.status != http.StatusUnauthorized {
			t.Errorf("HTTP %d, redirect %v: want a 401 and no redirect, so no code", o.status, o.redirect)
		}
	})
	t.Run("an unknown user", func(t *testing.T) {
		o := login(t, authParams(p, "s"), "nobody@example.com", "password")
		if o.redirect != nil || o.status != http.StatusUnauthorized {
			t.Errorf("HTTP %d, redirect %v: want a 401 and no redirect", o.status, o.redirect)
		}
	})
	t.Run("two users get two different identities", func(t *testing.T) {
		subject := func(user string) string {
			q := newPKCE(t)
			o := login(t, authParams(q, "s"), user, "password")
			tok := exchange(t, o.code(), q.verifier, redirectURI)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			v, err := auth.NewOIDCVerifier(ctx, issuer(), "", clientID)
			if err != nil {
				t.Fatal(err)
			}
			c, err := v.Verify(ctx, tok.fields["id_token"].(string))
			if err != nil {
				t.Fatal(err)
			}
			return c.Subject
		}
		a, b := subject("alice@example.com"), subject("bob@example.com")
		if a == "" || a == b {
			t.Errorf("alice's subject %q and bob's %q: want two different, non-empty subjects", a, b)
		}
	})
}

// The request above is written out by hand from the app. This reads the app's source and checks
// the three things the request is built from, so that if the app's client id, scopes or
// redirect URI change, this fails instead of the test above going on checking a request the
// app no longer makes. It needs no Dex.
func TestTheRequestMatchesTheAppsSource(t *testing.T) {
	src, err := os.ReadFile("../../mobile/src/auth/oidc.ts")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	m := regexp.MustCompile(`const CLIENT_ID = "([^"]+)"`).FindStringSubmatch(text)
	if m == nil || m[1] != clientID {
		t.Errorf("the app's CLIENT_ID is %v, this test uses %q", m, clientID)
	}
	m = regexp.MustCompile(`const SCOPES = \[([^\]]*)\]`).FindStringSubmatch(text)
	if m == nil {
		t.Fatal("SCOPES not found in mobile/src/auth/oidc.ts")
	}
	var scopes []string
	for _, part := range strings.Split(m[1], ",") {
		scopes = append(scopes, strings.Trim(strings.TrimSpace(part), `"`))
	}
	if got := strings.Join(scopes, " "); got != scope {
		t.Errorf("the app asks for scopes %q, this test uses %q", got, scope)
	}
	m = regexp.MustCompile(`makeRedirectUri\(\{ scheme: "([^"]+)", path: "([^"]+)" \}\)`).FindStringSubmatch(text)
	if m == nil || m[1]+"://"+m[2] != redirectURI {
		t.Errorf("the app's redirect URI is %v, this test uses %q", m, redirectURI)
	}
	if !strings.Contains(text, "usePKCE: true") {
		t.Error("the app no longer sets usePKCE: true")
	}
	// The same URI must be the one Dex has registered (a public client matches exactly).
	cfg, err := os.ReadFile("../../deploy/dex/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "- "+redirectURI) {
		t.Errorf("deploy/dex/config.yaml does not register %q", redirectURI)
	}
	app, err := os.ReadFile("../../mobile/app.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(app), `"scheme": "`+strings.SplitN(redirectURI, "://", 2)[0]+`"`) {
		t.Errorf("mobile/app.json does not declare the URL scheme of %q", redirectURI)
	}
}

// ---- refresh tokens (M10.21) ----
//
// The app asks for offline_access and keeps a refresh token. Dex v2.46.0 and v2.46.1 deadlocked
// on any refresh with in-memory storage, which is why deploy/dex/config.yaml uses Postgres; these
// tests would fail by timeout on a Dex that has that bug.

// scopeWithoutRefresh is what the app asked for before M10.21.
const scopeWithoutRefresh = "openid email profile"

// refreshLogin logs in as alice asking for offline_access and returns the token response.
func refreshLogin(t *testing.T) tokenResult {
	t.Helper()
	p := newPKCE(t)
	o := login(t, authParams(p, "state-"+p.verifier[:8]), "alice@example.com", "password")
	if o.redirect == nil || o.code() == "" {
		t.Fatalf("a login asking for offline_access gave no code: HTTP %d, redirect %v", o.status, o.redirect)
	}
	tok := exchange(t, o.code(), p.verifier, redirectURI)
	if tok.status != http.StatusOK {
		t.Fatalf("exchange = %d %v", tok.status, tok.fields)
	}
	return tok
}

func verifyIDToken(t *testing.T, idToken string) auth.Claims {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, err := auth.NewOIDCVerifier(ctx, issuer(), "", clientID)
	if err != nil {
		t.Fatalf("NewOIDCVerifier: %v", err)
	}
	c, err := v.Verify(ctx, idToken)
	if err != nil {
		t.Fatalf("the api's verifier rejected the ID token: %v", err)
	}
	return c
}

// idTokenLifetime is exp - iat of a JWT, read without verifying (the verifier does that).
func idTokenLifetime(t *testing.T, idToken string) time.Duration {
	t.Helper()
	parts := strings.Split(idToken, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ Exp, Iat float64 }
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return time.Duration(c.Exp-c.Iat) * time.Second
}

func TestRefresh_OnlyWithOfflineAccess(t *testing.T) {
	requireDex(t)
	// Without the scope there is no refresh token (the scopes the app used before 10.21); with it
	// (the app's request now) there is one.
	p := newPKCE(t)
	params := authParams(p, "s")
	params.Set("scope", scopeWithoutRefresh)
	o := login(t, params, "alice@example.com", "password")
	if o.redirect == nil || o.code() == "" {
		t.Fatalf("a login without offline_access gave no code: HTTP %d", o.status)
	}
	plain := exchange(t, o.code(), p.verifier, redirectURI)
	if _, ok := plain.fields["refresh_token"]; ok {
		t.Errorf("a refresh token was issued without offline_access")
	}
	if tok := refreshLogin(t); tok.fields["refresh_token"] == nil || tok.fields["refresh_token"] == "" {
		t.Errorf("no refresh token with offline_access: %v", tok.fields)
	}
}

func TestRefresh_GivesANewIDTokenTheApiAcceptsAndRotatesTheRefreshToken(t *testing.T) {
	requireDex(t)
	first := refreshLogin(t)
	rt1 := first.fields["refresh_token"].(string)
	id1 := first.fields["id_token"].(string)
	if d := idTokenLifetime(t, id1); d != time.Hour {
		t.Errorf("the ID token lives %v, want 1h (deploy/dex/config.yaml expiry.idTokens)", d)
	}

	second := refresh(t, rt1, clientID)
	if second.status != http.StatusOK {
		t.Fatalf("refresh = %d %v", second.status, second.fields)
	}
	id2, _ := second.fields["id_token"].(string)
	rt2, _ := second.fields["refresh_token"].(string)
	if id2 == "" || id2 == id1 {
		t.Errorf("the refresh gave no new ID token")
	}
	if rt2 == "" || rt2 == rt1 {
		t.Errorf("the refresh token was not rotated: %q", rt2)
	}
	c1, c2 := verifyIDToken(t, id1), verifyIDToken(t, id2)
	if c1.Subject != c2.Subject || c2.Email != "alice@example.com" || c2.Issuer != issuer() {
		t.Errorf("claims changed across a refresh: %+v then %+v", c1, c2)
	}
	if d := idTokenLifetime(t, id2); d != time.Hour {
		t.Errorf("the refreshed ID token lives %v, want 1h", d)
	}
}

// What the app must be able to rely on: an old refresh token is dead, and replaying it does not
// kill the current one (so a stale request cannot log the user out by itself).
func TestRefresh_AReplayedTokenIsRefusedAndTheCurrentOneKeepsWorking(t *testing.T) {
	requireDex(t)
	first := refreshLogin(t)
	rt1 := first.fields["refresh_token"].(string)
	second := refresh(t, rt1, clientID)
	if second.status != http.StatusOK {
		t.Fatalf("first refresh = %d %v", second.status, second.fields)
	}
	rt2 := second.fields["refresh_token"].(string)

	replay := refresh(t, rt1, clientID)
	if replay.status != http.StatusBadRequest || replay.fields["error"] != "invalid_grant" || replay.fields["id_token"] != nil {
		t.Errorf("replaying the used token = %d %v, want 400 invalid_grant and no token", replay.status, replay.fields)
	}
	third := refresh(t, rt2, clientID)
	if third.status != http.StatusOK || third.fields["id_token"] == nil {
		t.Errorf("after the replay the current token = %d %v, want it to still work", third.status, third.fields)
	}
}

func TestRefresh_Refusals(t *testing.T) {
	requireDex(t)
	rt := refreshLogin(t).fields["refresh_token"].(string)

	if r := refresh(t, rt, "some-other-client"); r.status != http.StatusUnauthorized || r.fields["id_token"] != nil {
		t.Errorf("another client id = %d %v, want 401 and no token", r.status, r.fields)
	}
	if r := refresh(t, "not-a-refresh-token", clientID); r.status != http.StatusBadRequest || r.fields["error"] != "invalid_grant" {
		t.Errorf("a garbage token = %d %v, want 400 invalid_grant", r.status, r.fields)
	}
	if r := postToken(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}}); r.status != http.StatusBadRequest {
		t.Errorf("no token at all = %d %v, want 400", r.status, r.fields)
	}
	// None of those used up the real one.
	if r := refresh(t, rt, clientID); r.status != http.StatusOK {
		t.Errorf("the real token after the refusals = %d %v, want 200", r.status, r.fields)
	}
}
