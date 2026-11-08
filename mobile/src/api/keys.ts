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

/** The api's rule (internal/idempotency): 1-255 characters, each visible ASCII (0x21-0x7e). */
export function isValidIdempotencyKey(key: string): boolean {
  return /^[\x21-\x7e]{1,255}$/.test(key);
}
