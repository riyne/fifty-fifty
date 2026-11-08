// The app does NOT verify ID tokens: the api does (signature, issuer, audience, expiry). The
// client only reads `exp`, to avoid sending a token that is already known to be dead and to
// show the login screen at startup instead of a first request that fails with 401.

function base64UrlToString(part: string): string | null {
  if (!/^[A-Za-z0-9_-]+$/.test(part)) return null;
  const padded = part.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(part.length / 4) * 4, "=");
  try {
    return atob(padded);
  } catch {
    return null;
  }
}

/** The `exp` claim (seconds since the epoch) of a JWT, or null if the token is not readable. */
export function tokenExpiry(token: string): number | null {
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  const payload = base64UrlToString(parts[1]);
  if (payload === null) return null;
  try {
    const claims: unknown = JSON.parse(payload);
    const exp = (claims as { exp?: unknown } | null)?.exp;
    return typeof exp === "number" && Number.isFinite(exp) ? exp : null;
  } catch {
    return null;
  }
}

/**
 * True if the token has a readable expiry that is more than `skewSeconds` away. A token with
 * no readable expiry is not usable: the api's tokens always have one.
 */
export function isUsable(token: string, nowSeconds: number, skewSeconds = 30): boolean {
  const exp = tokenExpiry(token);
  return exp !== null && exp - nowSeconds > skewSeconds;
}
