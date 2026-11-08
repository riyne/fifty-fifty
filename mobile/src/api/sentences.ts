import { formatMoney, type Currency } from "./money";

// The wording of balances and suggested transfers, apart from the screens so it can be tested.
// `name` is a display name from the api; for the signed-in user the sentence says "you".

/** "Alice is owed $5.00 CAD", "You owe $5.00 CAD", "Bob is settled up". */
export function balanceSentence(name: string, isMe: boolean, minor: number, currency: Currency): string {
  if (minor > 0) return `${isMe ? "You are" : `${name} is`} owed ${formatMoney(minor, currency)}`;
  if (minor < 0) return `${isMe ? "You owe" : `${name} owes`} ${formatMoney(-minor, currency)}`;
  return `${isMe ? "You are" : `${name} is`} settled up`;
}

/** "You pay Bob $12.50 CAD", "Bob pays you $12.50 CAD", "Bob pays Carol $12.50 CAD". */
export function transferSentence(
  from: { name: string; isMe: boolean },
  to: { name: string; isMe: boolean },
  amountMinor: number,
  currency: Currency,
): string {
  const payer = from.isMe ? "You pay" : `${from.name} pays`;
  const payee = to.isMe ? "you" : to.name;
  return `${payer} ${payee} ${formatMoney(amountMinor, currency)}`;
}
