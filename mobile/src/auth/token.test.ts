/**
 * @jest-environment node
 */
import { b64url, fakeJwt } from "./fakeJwt";
import { isUsable, tokenExpiry } from "./token";

describe("tokenExpiry", () => {
  it("reads exp", () => {
    expect(tokenExpiry(fakeJwt({ sub: "u", exp: 1800000000 }))).toBe(1800000000);
  });

  it("reads exp when other claims hold non-ASCII text (a display name)", () => {
    expect(tokenExpiry(fakeJwt({ name: "Zoë Müller 山田", exp: 1800000000 }))).toBe(1800000000);
  });

  it("handles payloads whose base64url length needs 0, 1, 2 and 3 padding characters", () => {
    for (let i = 0; i < 8; i++) {
      expect(tokenExpiry(fakeJwt({ pad: "x".repeat(i), exp: 1234567890 }))).toBe(1234567890);
    }
  });

  it.each([
    ["not three parts", "abc.def"],
    ["four parts", "a.b.c.d"],
    ["empty", ""],
    ["payload is not base64url", "a.@@@.c"],
    ["payload is not JSON", `a.${b64url("not json")}.c`],
    ["payload is JSON null", `a.${b64url("null")}.c`],
    ["payload is an array", `a.${b64url("[1]")}.c`],
    ["no exp", fakeJwt({ sub: "u" })],
    ["exp is a string", fakeJwt({ exp: "1800000000" })],
    ["exp is null", fakeJwt({ exp: null })],
  ])("returns null when %s", (_name, token) => {
    expect(tokenExpiry(token)).toBeNull();
  });
});

describe("isUsable", () => {
  const now = 1_000_000;

  it("is true when the token expires after the skew", () => {
    expect(isUsable(fakeJwt({ exp: now + 3600 }), now)).toBe(true);
    expect(isUsable(fakeJwt({ exp: now + 31 }), now)).toBe(true);
  });

  it("is false at or inside the 30 second skew, and when expired", () => {
    expect(isUsable(fakeJwt({ exp: now + 30 }), now)).toBe(false);
    expect(isUsable(fakeJwt({ exp: now + 1 }), now)).toBe(false);
    expect(isUsable(fakeJwt({ exp: now }), now)).toBe(false);
    expect(isUsable(fakeJwt({ exp: now - 3600 }), now)).toBe(false);
  });

  it("honors a different skew", () => {
    expect(isUsable(fakeJwt({ exp: now + 5 }), now, 0)).toBe(true);
    expect(isUsable(fakeJwt({ exp: now + 5 }), now, 10)).toBe(false);
  });

  it("is false for a token with no readable expiry", () => {
    expect(isUsable("garbage", now)).toBe(false);
    expect(isUsable(fakeJwt({ sub: "u" }), now)).toBe(false);
  });
});
