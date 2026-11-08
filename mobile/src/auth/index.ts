import { useSyncExternalStore } from "react";

import { keychainRefreshStore, keychainStore } from "./keychain";
import { refreshWithOidc } from "./oidc";
import { createSession, type SessionState } from "./session";

/** The one login state of the app. */
export const session = createSession(keychainStore, undefined, { store: keychainRefreshStore, refresh: refreshWithOidc });

export function useSession(): SessionState {
  return useSyncExternalStore(session.subscribe, session.getState, session.getState);
}
