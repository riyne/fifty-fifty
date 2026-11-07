/**
 * @jest-environment node
 */
import { formatMinor, formatMoney } from "./money";

describe("formatMinor", () => {
  it.each([
    [0, "0.00"],
    [1, "0.01"],
    [9, "0.09"],
    [10, "0.10"],
    [99, "0.99"],
    [100, "1.00"],
    [123456, "1,234.56"],
    [100000000, "1,000,000.00"],
    [999999999, "9,999,999.99"],
    [-5, "-0.05"],
    [-100, "-1.00"],
    [-123456, "-1,234.56"],
    [Number.MAX_SAFE_INTEGER, "90,071,992,547,409.91"],
    [Number.MIN_SAFE_INTEGER, "-90,071,992,547,409.91"],
  ])("%d -> %s", (minor, text) => {
    expect(formatMinor(minor)).toBe(text);
  });

  it("rejects what is not a safe integer instead of rounding it", () => {
    for (const bad of [0.5, 12.34, NaN, Infinity, -Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      expect(() => formatMinor(bad)).toThrow(RangeError);
    }
  });

  it("agrees with BigInt arithmetic on 50,000 seeded random amounts", () => {
    let seed = 12345;
    const next = () => {
      seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
      return seed;
    };
    for (let i = 0; i < 50000; i++) {
      // up to about 2^52, sign included, built from two 32-bit draws
      const magnitude = (next() % 0x100000) * 0x100000000 + next();
      const minor = next() % 2 === 0 ? magnitude : -magnitude;
      const big = BigInt(minor);
      const abs = big < 0n ? -big : big;
      const expected =
        (big < 0n ? "-" : "") +
        (abs / 100n).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",") +
        "." +
        (abs % 100n).toString().padStart(2, "0");
      expect(formatMinor(minor)).toBe(expected);
    }
  });
});

describe("formatMoney", () => {
  it("puts the sign before the dollar sign and always shows the currency", () => {
    expect(formatMoney(1250, "CAD")).toBe("$12.50 CAD");
    expect(formatMoney(-1250, "USD")).toBe("-$12.50 USD");
    expect(formatMoney(0, "USD")).toBe("$0.00 USD");
  });
});
