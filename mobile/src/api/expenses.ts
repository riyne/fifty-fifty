import type { Expense, ExpenseItem, ExpenseStatus } from "./types";

// What the expense screens decide from an expense, apart from the screens so it can be tested.
// These only decide what to OFFER. The api enforces every rule again (payer only, state, claims
// racing a finalize), and a screen shows its answer when the two disagree.

export function statusLabel(status: ExpenseStatus): string {
  switch (status) {
    case "parsing":
      return "Reading the receipt";
    case "needs_review":
      return "Waiting for the payer to review";
    case "claiming":
      return "Open for claims";
    case "finalized":
      return "Finalized";
    case "void":
      return "Voided";
  }
}

export function canClaim(expense: Pick<Expense, "status">): boolean {
  return expense.status === "claiming";
}

/** Only the payer reviews, and only a receipt the worker left in `needs_review`. */
export function canReview(expense: Pick<Expense, "status" | "payer_id">, meId: string): boolean {
  return expense.status === "needs_review" && expense.payer_id === meId;
}

export function canFinalize(expense: Pick<Expense, "status" | "payer_id">, meId: string): boolean {
  return expense.status === "claiming" && expense.payer_id === meId;
}

/** The payer or the group owner, and only a finalized expense. */
export function canVoid(expense: Pick<Expense, "status" | "payer_id">, meId: string, role: "owner" | "member"): boolean {
  return expense.status === "finalized" && (expense.payer_id === meId || role === "owner");
}

export function unclaimedCount(expense: Pick<Expense, "items">): number {
  return expense.items.filter((item) => item.claimed_by.length === 0).length;
}

/**
 * The body for finalize. Items nobody claimed are split evenly among `everyone` (the group's
 * members), which is the only way the api finalizes with unclaimed items; when every item has a
 * claimant the field is left out, so the request does not depend on who is in the group.
 */
export function finalizeBody(expense: Pick<Expense, "items">, everyone: readonly string[]): { split_rest_among?: string[] } {
  return unclaimedCount(expense) === 0 ? {} : { split_rest_among: [...everyone] };
}

/** "Nobody yet", "You", "Bob", "You and Bob", "You, Bob and Carol". Names come from `nameOf`. */
export function claimSentence(item: Pick<ExpenseItem, "claimed_by">, meId: string, nameOf: (memberId: string) => string): string {
  if (item.claimed_by.length === 0) return "Nobody yet";
  const names = item.claimed_by.map((id) => (id === meId ? "You" : nameOf(id)));
  // "You" first, the others in the api's order
  names.sort((a, b) => Number(b === "You") - Number(a === "You"));
  if (names.length === 1) return names[0];
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}
