// Money on the wire is an integer in minor units (cents). The client never turns it into a
// float: the whole and fraction parts come from integer remainder, not from division.

export type Currency = "CAD" | "USD";

function assertMinor(minor: number): void {
  if (!Number.isSafeInteger(minor)) {
    throw new RangeError(`amount is not a safe integer: ${minor}`);
  }
}

/** 123456 -> "1,234.56"; -5 -> "-0.05". */
export function formatMinor(minor: number): string {
  assertMinor(minor);
  const negative = minor < 0;
  const abs = Math.abs(minor);
  const cents = abs % 100;
  const whole = (abs - cents) / 100; // exact: abs - cents is a multiple of 100
  const grouped = String(whole).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return `${negative ? "-" : ""}${grouped}.${String(cents).padStart(2, "0")}`;
}

/** 123456, "CAD" -> "$1,234.56 CAD". The code is always shown: a group has one currency. */
export function formatMoney(minor: number, currency: Currency): string {
  const text = formatMinor(minor);
  return text.startsWith("-") ? `-$${text.slice(1)} ${currency}` : `$${text} ${currency}`;
}
