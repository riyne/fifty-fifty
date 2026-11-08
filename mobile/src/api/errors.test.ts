/**
 * @jest-environment node
 */
import { ApiError, describeError, NetworkError } from "./errors";

describe("describeError", () => {
  it("explains a network failure without the cause", () => {
    expect(describeError(new NetworkError(new TypeError("Network request failed")))).toBe(
      "Can't reach the server. Is the api running?",
    );
  });

  it("shows the api's message for a 4xx", () => {
    expect(describeError(new ApiError(404, "not_found", "group not found"))).toBe("group not found");
  });

  it("lets a screen replace the message for one status", () => {
    const e = new ApiError(404, "not_found", "not found");
    expect(describeError(e, { 404: "That code didn't work." })).toBe("That code didn't work.");
    expect(describeError(new ApiError(400, "bad", "bad body"), { 404: "x" })).toBe("bad body");
  });

  it("says when to retry after a 429, with and without a hint", () => {
    expect(describeError(new ApiError(429, "rate_limited", "x", 7))).toBe("Too many requests. Try again in 7 s.");
    expect(describeError(new ApiError(429, "rate_limited", "x"))).toBe("Too many requests. Try again shortly.");
  });

  it("does not show a server error's text", () => {
    expect(describeError(new ApiError(500, "internal", "pq: secret detail"))).toBe("The server had a problem. Try again.");
  });

  it("handles a 401 and anything else", () => {
    expect(describeError(new ApiError(401, "unauthorized", "x"))).toBe("Your session ended. Sign in again.");
    expect(describeError(new Error("boom"))).toBe("Something went wrong.");
    expect(describeError("boom")).toBe("Something went wrong.");
  });
});
