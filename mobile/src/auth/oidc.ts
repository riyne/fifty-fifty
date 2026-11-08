import * as AuthSession from "expo-auth-session";

// Authorization Code + PKCE (S256) against Dex, in the system browser (an
// ASWebAuthenticationSession, so the app never sees the password). Dex's client `fifty-fifty`
// is public (no secret) and must list REDIRECT_URI (deploy/dex/config.yaml).
export const OIDC_ISSUER = process.env.EXPO_PUBLIC_OIDC_ISSUER ?? "http://localhost:5556/dex";
const CLIENT_ID = "fifty-fifty";
const SCOPES = ["openid", "email", "profile"]; // no offline_access: no refresh token
export const REDIRECT_URI = AuthSession.makeRedirectUri({ scheme: "fiftyfifty", path: "auth" });

/**
 * Runs the login and returns the ID token, or null if the user closed the browser. Throws an
 * Error with a message that is safe to show on any other failure. `state` is generated and
 * checked by AuthRequest, and the code verifier stays in memory for the exchange.
 */
export async function signInWithOidc(): Promise<string | null> {
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
  return tokens.idToken;
}
