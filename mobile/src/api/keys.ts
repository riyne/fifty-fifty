import { randomUUID } from "expo-crypto";

/**
 * A new Idempotency-Key. Make one per user ACTION (the tap on "Finalize"), keep it for as long
 * as that action is being retried, and make a new one for the next action. The api stores the
 * answer under the key for 24 hours and replays it, so reusing a key for a different request
 * is refused (422), and a new key for a retry would do the action twice.
 */
export function newIdempotencyKey(): string {
  return randomUUID();
}

/**
 * For a form whose request is fully determined by one value (a join code): the same value gets
 * the same key, so tapping the button again after a lost answer is a retry that the api
 * replays; a different value gets a new key, because the api refuses one key for two different
 * requests (422). Make one per screen, not one for the app.
 */
export function keyedByValue(make: () => string = newIdempotencyKey): (value: string) => string {
  let last: { value: string; key: string } | null = null;
  return (value) => {
    if (last === null || last.value !== value) last = { value, key: make() };
    return last.key;
  };
}

/**
 * For an action that changes state and whose answer the api stores (finalize, void): one tap is
 * one action. `keyFor(value)` gives the key for the request now being sent; `value` describes the
 * request (its body), so a changed request gets a new key (the api refuses one key for two
 * different requests, 422). A retry after the request got no answer (network error) or a 5xx
 * (not stored) must keep the key, so the api does the action once. Once the api answered with
 * anything below 500 the answer is stored under the key and replayed for 24 hours, including a
 * 409: the next tap is a new action and needs a new key, so call `answered()`.
 * Make one per screen and action.
 */
export function createActionKeys(make: () => string = newIdempotencyKey) {
  let current: { value: string; key: string } | null = null;
  return {
    keyFor(value: string): string {
      if (current === null || current.value !== value) current = { value, key: make() };
      return current.key;
    },
    answered(): void {
      current = null;
    },
  };
}

/** The api's rule (internal/idempotency): 1-255 characters, each visible ASCII (0x21-0x7e). */
export function isValidIdempotencyKey(key: string): boolean {
  return /^[\x21-\x7e]{1,255}$/.test(key);
}
