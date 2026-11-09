import { isUsable } from "./token";

export type SessionState =
  | { status: "loading" } // the stored token has not been read yet
  | { status: "signedOut" }
  | { status: "signedIn"; token: string };

/** Where the token is kept between launches (the Keychain in the app, a fake in tests). */
export interface TokenStore {
  load(): Promise<string | null>;
  save(token: string): Promise<void>;
  clear(): Promise<void>;
}

/** Where the refresh token is kept (a second Keychain item in the app, a fake in tests). */
export interface RefreshStore {
  load(): Promise<string | null>;
  save(token: string): Promise<void>;
  clear(): Promise<void>;
}

/** What a refresh gives back. `refreshToken` is the NEW one (Dex rotates it on every use); null keeps the old. */
export type Tokens = { idToken: string; refreshToken: string | null };

/** The provider refused the refresh token for good (revoked, expired, already used): sign out. Any other error is transient. */
export class RefreshRejectedError extends Error {}

export type Refresher = (refreshToken: string) => Promise<Tokens>;

/** Optional refresh support. Without it the session behaves as before: an expired ID token means a new login. */
export interface Refreshing {
  store: RefreshStore;
  refresh: Refresher;
  /** Refresh when the ID token has less than this many seconds left. Default 60. */
  skewSeconds?: number;
  /** A 401 for a token that was itself obtained by a refresh less than this many seconds ago ends the session instead of refreshing again. Default 30. */
  minRefreshIntervalSeconds?: number;
}

/**
 * The login state: one token in memory, mirrored in a TokenStore. The state is what the
 * screens react to, and `getToken` is what the api client calls for every request.
 *
 * Rules:
 * - `restore` signs in only with a stored token that is still usable; anything else is
 *   removed from the store and the user is signed out. A store that fails means signed out.
 * - `signOut` changes the state first and clears the store after, so no request goes out with
 *   the old token while the Keychain call is pending.
 * - `signIn` keeps the user signed in for this run even if saving fails (the next launch then
 *   asks for a login again); an unusable token is refused and nothing changes.
 * - `expire(sent)` ends the session only if `sent` is the token currently in use, so the late
 *   401 of a request made under an old login cannot end a new one.
 *
 * With `refreshing` there is also a refresh token, and:
 * - `getTokenForRequest` returns the ID token, first refreshing it if it is within `skewSeconds`
 *   of expiry. Concurrent callers share ONE refresh (the provider rotates the refresh token, so
 *   a second refresh with the same token would be refused).
 * - A refresh the provider refuses for good (RefreshRejectedError) signs out; any other failure
 *   (no network, a 5xx) keeps the session and the stored tokens and fails only that call.
 * - A refresh that finishes after a sign-out or a newer login is discarded.
 * - A 401 for the token in use tries one refresh; a 401 for a token that a refresh made less
 *   than `minRefreshIntervalSeconds` ago ends the session (something other than expiry is wrong).
 * - `restore` with an expired ID token but a refresh token refreshes instead of signing out.
 */
export function createSession(
  store: TokenStore,
  nowSeconds: () => number = () => Date.now() / 1000,
  refreshing?: Refreshing,
) {
  let state: SessionState = { status: "loading" };
  const listeners = new Set<() => void>();
  let refreshToken: string | null = null;
  let generation = 0; // changes with every sign-in and sign-out
  let inFlight: Promise<boolean> | null = null;
  let lastRefreshAt = -Infinity;
  const skew = refreshing?.skewSeconds ?? 60;
  const minInterval = refreshing?.minRefreshIntervalSeconds ?? 30;

  function set(next: SessionState) {
    state = next;
    listeners.forEach((listener) => listener());
  }

  async function clearStore() {
    try {
      await store.clear();
    } catch {
      // nothing more can be done; the token is out of memory and the next restore re-checks it
    }
  }

  async function restore(): Promise<void> {
    let stored: string | null = null;
    try {
      stored = await store.load();
    } catch {
      stored = null;
    }
    let storedRefresh: string | null = null;
    if (refreshing) {
      try {
        storedRefresh = await refreshing.store.load();
      } catch {
        storedRefresh = null;
      }
    }
    if (state.status !== "loading") return; // a login or logout got there first
    if (stored !== null && isUsable(stored, nowSeconds())) {
      refreshToken = storedRefresh;
      set({ status: "signedIn", token: stored });
      return;
    }
    if (refreshing && storedRefresh !== null) {
      // The ID token is gone or expired but there is a refresh token: refresh instead of a login.
      refreshToken = storedRefresh;
      generation++;
      const gen = generation;
      try {
        const next = await refreshing.refresh(storedRefresh);
        if (state.status !== "loading" || gen !== generation) return;
        if (!isUsable(next.idToken, nowSeconds())) throw new RefreshRejectedError("the provider returned an unusable token");
        applyRefresh(next);
        return;
      } catch (error) {
        if (state.status !== "loading" || gen !== generation) return;
        if (error instanceof RefreshRejectedError) {
          refreshToken = null;
          set({ status: "signedOut" });
          await clearStore();
          await clearRefresh();
          return;
        }
        if (stored !== null) {
          // No network, say: stay signed in with the old token; the first request tries again.
          set({ status: "signedIn", token: stored });
          return;
        }
        refreshToken = null;
        set({ status: "signedOut" });
        return;
      }
    }
    set({ status: "signedOut" });
    if (stored !== null) await clearStore();
    if (refreshing && storedRefresh !== null) await clearRefresh();
  }

  async function clearRefresh() {
    try {
      await refreshing?.store.clear();
    } catch {
      // as for clearStore
    }
  }

  /** Take a successful refresh into the state and the stores. */
  function applyRefresh(next: Tokens) {
    if (next.refreshToken !== null) refreshToken = next.refreshToken;
    lastRefreshAt = nowSeconds();
    set({ status: "signedIn", token: next.idToken });
    void store.save(next.idToken).catch(() => {});
    if (refreshToken !== null) void refreshing?.store.save(refreshToken).catch(() => {});
  }

  /**
   * One refresh at a time. Resolves true if the session now holds a fresh token, false if the
   * session ended or changed meanwhile; throws if the refresh failed for a reason that leaves the
   * session as it was (no network, a 5xx).
   */
  function refreshOnce(): Promise<boolean> {
    if (inFlight) return inFlight;
    const rt = refreshToken;
    if (!refreshing || rt === null || state.status !== "signedIn") return Promise.resolve(false);
    const gen = generation;
    inFlight = (async () => {
      try {
        const next = await refreshing.refresh(rt);
        if (gen !== generation || state.status !== "signedIn") return false; // signed out or in again meanwhile
        if (!isUsable(next.idToken, nowSeconds())) throw new RefreshRejectedError("the provider returned an unusable token");
        applyRefresh(next);
        return true;
      } catch (error) {
        if (error instanceof RefreshRejectedError && gen === generation) {
          await signOut();
          return false;
        }
        throw error;
      } finally {
        inFlight = null;
      }
    })();
    return inFlight;
  }

  async function signIn(token: string, newRefreshToken: string | null = null): Promise<void> {
    if (!isUsable(token, nowSeconds())) throw new Error("the provider returned an unusable token");
    generation++;
    refreshToken = refreshing ? newRefreshToken : null;
    lastRefreshAt = -Infinity;
    set({ status: "signedIn", token });
    try {
      await store.save(token);
      if (refreshing && newRefreshToken !== null) await refreshing.store.save(newRefreshToken);
    } catch {
      // signed in for this run only
    }
  }

  async function signOut(): Promise<void> {
    generation++;
    refreshToken = null;
    set({ status: "signedOut" });
    await clearStore();
    await clearRefresh();
  }

  async function expire(sentToken: string | null): Promise<void> {
    if (state.status !== "signedIn" || state.token !== sentToken) return;
    const recentlyRefreshed = nowSeconds() - lastRefreshAt < minInterval;
    if (refreshing && refreshToken !== null && !recentlyRefreshed) {
      try {
        await refreshOnce();
      } catch {
        // transient: stay signed in; the next request tries again
      }
      return;
    }
    await signOut();
  }

  /**
   * The token to send with a request: the ID token, refreshed first if it is about to expire.
   * Throws if the refresh failed for a transient reason (the request then fails like any network
   * error and the session is untouched); null when signed out.
   */
  async function getTokenForRequest(): Promise<string | null> {
    if (state.status !== "signedIn") return null;
    if (refreshing && refreshToken !== null && !isUsable(state.token, nowSeconds(), skew)) {
      await refreshOnce();
    }
    return state.status === "signedIn" ? state.token : null;
  }

  return {
    getState: (): SessionState => state,
    subscribe(listener: () => void): () => void {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    getToken: (): string | null => (state.status === "signedIn" ? state.token : null),
    getTokenForRequest,
    restore,
    signIn,
    signOut,
    expire,
  };
}

export type Session = ReturnType<typeof createSession>;
