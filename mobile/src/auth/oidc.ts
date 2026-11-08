import * as AuthSession from "expo-auth-session";

import { RefreshRejectedError, type Tokens } from "./session";

// Authorization Code + PKCE (S256) against Dex, in the system browser (an
// ASWebAuthenticationSession, so the app never sees the password). Dex's client `fifty-fifty`
// is public (no secret) and must list REDIRECT_URI (deploy/dex/config.yaml).
export const OIDC_ISSUER = process.env.EXPO_PUBLIC_OIDC_ISSUER ?? "http://localhost:5556/dex";
const CLIENT_ID = "fifty-fifty";
const SCOPES = ["openid", "email", "profile", "offline_access"]; // offline_access: Dex issues a refresh token (M10.21)
export const REDIRECT_URI = AuthSession.makeRedirectUri({ scheme: "fiftyfifty", path: "auth" });

/**
 * Runs the login and returns the ID token and the refresh token, or null if the user closed the browser. Throws an
 * Error with a message that is safe to show on any other failure. `state` is generated and
 * checked by AuthRequest, and the code verifier stays in memory for the exchange.
 */
export async function signInWithOidc(): Promise<Tokens | null> {
  const discovery = await AuthSession.fetchDiscoveryAsync(OIDC_ISSUER);
  const request = new AuthSession.AuthRequest({
    clientId: CLIENT_ID,
    scopes: SCOPES,
    redirectUri: REDIRECT_URI,
    usePKCE: true,
  });
  const result = await request.promptAsync(discovery);
  if (result.type === "cancel" || result.type === "dismiss") return null;
  if (result.type !== "success") {
    throw new Error(result.type === "error" ? `login failed: ${result.error?.code ?? "error"}` : "login did not finish");
  }
  if (!request.codeVerifier) throw new Error("login failed: no code verifier");
  const tokens = await AuthSession.exchangeCodeAsync(
    {
      clientId: CLIENT_ID,
      code: result.params.code,
      redirectUri: REDIRECT_URI,
      extraParams: { code_verifier: request.codeVerifier },
    },
    discovery,
  );
  if (!tokens.idToken) throw new Error("login failed: the provider returned no ID token");
  return { idToken: tokens.idToken, refreshToken: tokens.refreshToken ?? null };
}

// What Dex answers when a refresh token is no good for good: revoked, expired, already used (it
// rotates them), or not this client's. Anything else (no network, a 5xx) is transient.
const REJECTED = new Set(["invalid_grant", "invalid_client", "unauthorized_client", "invalid_request"]);

/** Exchanges the refresh token for a new ID token and the NEXT refresh token (Dex rotates it). */
export async function refreshWithOidc(refreshToken: string): Promise<Tokens> {
  const discovery = await AuthSession.fetchDiscoveryAsync(OIDC_ISSUER);
  try {
    const tokens = await AuthSession.refreshAsync({ clientId: CLIENT_ID, refreshToken }, discovery);
    if (!tokens.idToken) throw new RefreshRejectedError("the provider returned no ID token");
    return { idToken: tokens.idToken, refreshToken: tokens.refreshToken ?? null };
  } catch (error) {
    if (error instanceof AuthSession.TokenError && REJECTED.has(error.code)) throw new RefreshRejectedError(error.code);
    throw error;
  }
}
