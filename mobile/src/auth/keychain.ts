import * as SecureStore from "expo-secure-store";

import type { RefreshStore, TokenStore } from "./session";

const KEY = "id_token"; // SecureStore keys: letters, digits, ".", "-", "_"
const REFRESH_KEY = "refresh_token";

// THIS_DEVICE_ONLY keeps the token out of backups and device-to-device transfers; WHEN_UNLOCKED
// is enough because the app only needs it in the foreground. An ID token is about 1 KiB, under
// the roughly 2 KiB some iOS releases have refused.
const OPTIONS: SecureStore.SecureStoreOptions = {
  keychainAccessible: SecureStore.WHEN_UNLOCKED_THIS_DEVICE_ONLY,
};

/** The ID token in the iOS Keychain (kept across launches; it may also survive a reinstall). */
export const keychainStore: TokenStore = {
  load: () => SecureStore.getItemAsync(KEY, OPTIONS),
  save: (token) => SecureStore.setItemAsync(KEY, token, OPTIONS),
  clear: () => SecureStore.deleteItemAsync(KEY, OPTIONS),
};

/** The refresh token in its own Keychain item, with the same protection (this device only, when unlocked). */
export const keychainRefreshStore: RefreshStore = {
  load: () => SecureStore.getItemAsync(REFRESH_KEY, OPTIONS),
  save: (token) => SecureStore.setItemAsync(REFRESH_KEY, token, OPTIONS),
  clear: () => SecureStore.deleteItemAsync(REFRESH_KEY, OPTIONS),
};
