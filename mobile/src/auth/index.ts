import { useSyncExternalStore } from "react";

import { keychainStore } from "./keychain";
import { createSession, type SessionState } from "./session";

/** The one login state of the app. */
export const session = createSession(keychainStore);

export function useSession(): SessionState {
  return useSyncExternalStore(session.subscribe, session.getState, session.getState);
}
