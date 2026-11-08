/**
 * @jest-environment node
 */
import { createLatest } from "./latest";

describe("createLatest", () => {
  it("only the newest ticket is current, and cancel makes all stale", () => {
    const latest = createLatest();
    const a = latest.next();
    expect(latest.isCurrent(a)).toBe(true);
    const b = latest.next();
    expect(latest.isCurrent(a)).toBe(false);
    expect(latest.isCurrent(b)).toBe(true);
    latest.cancel();
    expect(latest.isCurrent(b)).toBe(false);
  });
});
