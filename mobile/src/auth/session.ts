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
 */
export function createSession(store: TokenStore, nowSeconds: () => number = () => Date.now() / 1000) {
  let state: SessionState = { status: "loading" };
  const listeners = new Set<() => void>();

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
    if (state.status !== "loading") return; // a login or logout got there first
    if (stored !== null && isUsable(stored, nowSeconds())) {
      set({ status: "signedIn", token: stored });
      return;
    }
    set({ status: "signedOut" });
    if (stored !== null) await clearStore();
  }

  async function signIn(token: string): Promise<void> {
    if (!isUsable(token, nowSeconds())) throw new Error("the provider returned an unusable token");
    set({ status: "signedIn", token });
    try {
      await store.save(token);
    } catch {
      // signed in for this run only
    }
  }

  async function signOut(): Promise<void> {
    set({ status: "signedOut" });
    await clearStore();
  }

  async function expire(sentToken: string | null): Promise<void> {
    if (state.status === "signedIn" && state.token === sentToken) await signOut();
  }

  return {
    getState: (): SessionState => state,
    subscribe(listener: () => void): () => void {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    getToken: (): string | null => (state.status === "signedIn" ? state.token : null),
    restore,
    signIn,
    signOut,
    expire,
  };
}

export type Session = ReturnType<typeof createSession>;
