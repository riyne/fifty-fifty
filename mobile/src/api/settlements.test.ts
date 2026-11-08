import { isOpen, settlementTitle, statusLine, statusWord } from "./settlements";
import type { SettlementStatus } from "./types";

const statuses: SettlementStatus[] = ["pending", "unknown", "succeeded", "failed"];
const names: Record<string, string> = { a: "alice", b: "bob" };
const nameOf = (id: string) => names[id] ?? "Someone";

describe("settlement states", () => {
  it("treats pending and unknown as still open, succeeded and failed as final", () => {
    expect(statuses.filter(isOpen)).toEqual(["pending", "unknown"]);
  });

  it("says every sentence is simulated where it could be taken for real", () => {
    for (const status of statuses) expect(statusLine({ status, failure_code: null })).toMatch(/simulated/);
  });

  it("names the decline reason when there is one", () => {
    expect(statusLine({ status: "failed", failure_code: "insufficient_funds" })).toBe(
      "Declined by the simulated provider (insufficient_funds). Nothing was moved.",
    );
    expect(statusLine({ status: "failed", failure_code: null })).toBe("Declined by the simulated provider. Nothing was moved.");
  });

  it("tells a person that an unclear answer keeps the money reserved", () => {
    expect(statusLine({ status: "unknown", failure_code: null })).toMatch(/reserved/);
  });

  it("has a short word for each state", () => {
    expect(statuses.map(statusWord)).toEqual(["Pending", "Being checked", "Paid", "Declined"]);
  });
});

describe("settlementTitle", () => {
  const s = { from_member_id: "a", to_member_id: "b", amount_minor: 1688 };
  it("words it from the signed-in user's point of view", () => {
    expect(settlementTitle(s, "a", nameOf, "CAD")).toBe("You to bob: $16.88 CAD");
    expect(settlementTitle(s, "b", nameOf, "CAD")).toBe("alice to you: $16.88 CAD");
    expect(settlementTitle(s, "x", nameOf, "USD")).toBe("alice to bob: $16.88 USD");
  });
});
