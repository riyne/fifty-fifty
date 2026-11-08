import { formatMoney } from "./money";
import type { Currency, Settlement, SettlementStatus } from "./types";

// Settlement is SIMULATED: the provider is a mock server and no real money moves. Every
// sentence here says so where a person could take it for real.

/** `pending` and `unknown` still reserve the money and can still turn into success or failure. */
export function isOpen(status: SettlementStatus): boolean {
  return status === "pending" || status === "unknown";
}

/** One sentence for where a settlement stands. */
export function statusLine(settlement: Pick<Settlement, "status" | "failure_code">): string {
  switch (settlement.status) {
    case "pending":
      return "Waiting for the simulated payment provider.";
    case "unknown":
      return "The simulated provider's answer was unclear. It will be checked again; the amount stays reserved until then.";
    case "succeeded":
      return "Paid (simulated). The balances are updated.";
    case "failed":
      return `Declined by the simulated provider${settlement.failure_code ? ` (${settlement.failure_code})` : ""}. Nothing was moved.`;
  }
}

/** "You to bob: $16.88 CAD", "alice to you: $16.88 CAD". Says nothing about the outcome: `statusLine` does. */
export function settlementTitle(
  settlement: Pick<Settlement, "from_member_id" | "to_member_id" | "amount_minor">,
  meId: string,
  nameOf: (memberId: string) => string,
  currency: Currency,
): string {
  const side = (memberId: string, first: boolean) => (memberId === meId ? (first ? "You" : "you") : nameOf(memberId));
  return `${side(settlement.from_member_id, true)} to ${side(settlement.to_member_id, false)}: ${formatMoney(settlement.amount_minor, currency)}`;
}

/** The short outcome for a list row. */
export function statusWord(status: SettlementStatus): string {
  switch (status) {
    case "pending":
      return "Pending";
    case "unknown":
      return "Being checked";
    case "succeeded":
      return "Paid";
    case "failed":
      return "Declined";
  }
}
