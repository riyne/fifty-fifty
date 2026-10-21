#!/usr/bin/env bash
# Print an ID token for a synthetic Dex user, using the real Auth Code + PKCE flow.
# Dev only. Usage: scripts/dev-token.sh [alice|bob|carol]   (default: alice)
#
#   TOKEN=$(scripts/dev-token.sh bob)
#   curl -H "Authorization: Bearer $TOKEN" localhost:8080/v1/me
set -euo pipefail

user="${1:-alice}"
issuer="${OIDC_ISSUER:-http://localhost:5556/dex}"
client_id="${OIDC_CLIENT_ID:-fifty-fifty}"
redirect_uri="http://localhost:8080/callback" # must match deploy/dex/config.yaml
password="password"                           # all synthetic users share it

jar="$(mktemp)"
trap 'rm -f "$jar"' EXIT

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
verifier="$(openssl rand 32 | b64url)"
challenge="$(printf '%s' "$verifier" | openssl dgst -sha256 -binary | b64url)"

auth_url="$issuer/auth?client_id=$client_id&response_type=code&scope=openid%20email%20profile"
auth_url+="&redirect_uri=$redirect_uri&state=s&code_challenge=$challenge&code_challenge_method=S256"

# 1. Follow Dex's redirects to the login form (cookies carry the auth request).
login_url="$(curl -sS -L -c "$jar" -b "$jar" -o /dev/null -w '%{url_effective}' "$auth_url")"

# 2. Submit credentials. Dex redirects to redirect_uri?code=...; nothing needs to
#    listen there, we only read the final URL (curl's connection error is expected).
final_url="$(curl -sS -L -c "$jar" -b "$jar" -o /dev/null -w '%{url_effective}' \
  --data-urlencode "login=$user@example.com" --data-urlencode "password=$password" \
  "$login_url" 2>/dev/null || true)"
code="$(printf '%s' "$final_url" | sed -n 's/.*[?&]code=\([^&]*\).*/\1/p')"
if [ -z "$code" ]; then
  echo "login failed (no code in redirect: $final_url)" >&2
  exit 1
fi

# 3. Exchange the code, proving possession of the PKCE verifier.
curl -sS -X POST "$issuer/token" \
  -d grant_type=authorization_code -d "code=$code" \
  -d "redirect_uri=$redirect_uri" -d "client_id=$client_id" -d "code_verifier=$verifier" |
  python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["id_token"])'
