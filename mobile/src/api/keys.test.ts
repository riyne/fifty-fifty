/**
 * @jest-environment node
 */
import { isValidIdempotencyKey, keyedByValue, newIdempotencyKey } from "./keys";

// jest-expo's mock of expo-crypto returns one constant, so uniqueness cannot be seen through it.
// Node's randomUUID stands in; what these tests cover is our wrapper and the api's key rule.
// The real expo-crypto is exercised only when the app runs on the simulator.
jest.mock("expo-crypto", () => ({ randomUUID: () => require("node:crypto").randomUUID() }));

describe("idempotency keys", () => {
  it("makes keys the api accepts, and a different one each time", () => {
    const keys = new Set<string>();
    for (let i = 0; i < 1000; i++) {
      const key = newIdempotencyKey();
      expect(isValidIdempotencyKey(key)).toBe(true);
      keys.add(key);
    }
    expect(keys.size).toBe(1000);
  });

  it("applies the api's rule: 1-255 visible ASCII characters", () => {
    expect(isValidIdempotencyKey("a")).toBe(true);
    expect(isValidIdempotencyKey("x".repeat(255))).toBe(true);
    expect(isValidIdempotencyKey("")).toBe(false);
    expect(isValidIdempotencyKey("x".repeat(256))).toBe(false);
    expect(isValidIdempotencyKey("has space")).toBe(false);
    expect(isValidIdempotencyKey("tab\t")).toBe(false);
    expect(isValidIdempotencyKey("naïve")).toBe(false);
    expect(isValidIdempotencyKey("~!")).toBe(true);
  });
});

describe("keyedByValue", () => {
  it("gives the same key for the same value and a new one for a different value", () => {
    let n = 0;
    const keyFor = keyedByValue(() => `key-${++n}`);
    expect(keyFor("AAAA")).toBe("key-1");
    expect(keyFor("AAAA")).toBe("key-1"); // a retry of the same request
    expect(keyFor("BBBB")).toBe("key-2");
    expect(keyFor("BBBB")).toBe("key-2");
    expect(keyFor("AAAA")).toBe("key-3"); // back to an earlier value: a new action
  });
});
