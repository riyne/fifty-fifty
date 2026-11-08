import { canClaim, canFinalize, canReview, canVoid, claimSentence, finalizeBody, statusLabel, unclaimedCount } from "./expenses";
import type { ExpenseStatus } from "./types";

const item = (...claimed_by: string[]) => ({ claimed_by });
const names: Record<string, string> = { a: "Alice", b: "Bob", c: "Carol" };
const nameOf = (id: string) => names[id] ?? "Someone";

describe("what an expense offers", () => {
  const statuses: ExpenseStatus[] = ["parsing", "needs_review", "claiming", "finalized", "void"];

  it("lets members claim only while the expense is open for claims", () => {
    expect(statuses.filter((status) => canClaim({ status }))).toEqual(["claiming"]);
  });

  it("lets only the payer finalize, and only an open expense", () => {
    for (const status of statuses) {
      expect(canFinalize({ status, payer_id: "a" }, "a")).toBe(status === "claiming");
      expect(canFinalize({ status, payer_id: "a" }, "b")).toBe(false);
    }
  });

  it("lets only the payer review, and only a receipt waiting for review", () => {
    for (const status of statuses) {
      expect(canReview({ status, payer_id: "a" }, "a")).toBe(status === "needs_review");
      expect(canReview({ status, payer_id: "a" }, "b")).toBe(false);
    }
  });

  it("lets the payer or the owner void, and only a finalized expense", () => {
    for (const status of statuses) {
      const finalized = status === "finalized";
      expect(canVoid({ status, payer_id: "a" }, "a", "member")).toBe(finalized);
      expect(canVoid({ status, payer_id: "a" }, "b", "owner")).toBe(finalized);
      expect(canVoid({ status, payer_id: "a" }, "b", "member")).toBe(false);
    }
  });

  it("has a sentence for every status", () => {
    expect(statuses.map(statusLabel)).toEqual([
      "Reading the receipt",
      "Waiting for the payer to review",
      "Open for claims",
      "Finalized",
      "Voided",
    ]);
  });
});

describe("finalize body", () => {
  it("counts the items nobody claimed", () => {
    expect(unclaimedCount({ items: [item(), item("a"), item(), item("a", "b")] } as never)).toBe(2);
    expect(unclaimedCount({ items: [] } as never)).toBe(0);
  });

  it("is empty when every item has a claimant, whoever is in the group", () => {
    expect(finalizeBody({ items: [item("a"), item("b", "c")] } as never, ["a", "b", "c"])).toEqual({});
  });

  it("splits unclaimed items among everyone in the group", () => {
    expect(finalizeBody({ items: [item("a"), item()] } as never, ["a", "b", "c"])).toEqual({
      split_rest_among: ["a", "b", "c"],
    });
  });
});

describe("claimSentence", () => {
  it("says who holds an item, with 'You' first", () => {
    expect(claimSentence(item(), "a", nameOf)).toBe("Nobody yet");
    expect(claimSentence(item("a"), "a", nameOf)).toBe("You");
    expect(claimSentence(item("b"), "a", nameOf)).toBe("Bob");
    expect(claimSentence(item("a", "b"), "a", nameOf)).toBe("You and Bob");
    expect(claimSentence(item("a", "b"), "b", nameOf)).toBe("You and Alice");
    expect(claimSentence(item("a", "b", "c"), "c", nameOf)).toBe("You, Alice and Bob");
    expect(claimSentence(item("a", "b", "c"), "x", nameOf)).toBe("Alice, Bob and Carol");
    expect(claimSentence(item("z"), "a", nameOf)).toBe("Someone");
  });
});
