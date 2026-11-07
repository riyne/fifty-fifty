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

/** The api's rule (internal/idempotency): 1-255 characters, each visible ASCII (0x21-0x7e). */
export function isValidIdempotencyKey(key: string): boolean {
  return /^[\x21-\x7e]{1,255}$/.test(key);
}
