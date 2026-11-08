import type { components, paths } from "./schema";
import type { Expense } from "./types";

// The payer's review of a receipt the worker could not finish (status `needs_review`): fix the
// items, tax and tip, then confirm. This file turns what the person typed into the PATCH body
// the api wants, apart from the screen so it can be tested. Money is typed as text ("12.50")
// and becomes integer minor units by string handling only, never through a float.

export type EditBody = Omit<
  paths["/v1/groups/{id}/expenses/{expenseId}"]["patch"]["requestBody"]["content"]["application/json"],
  "version"
>;
export type ConfirmedExpense = components["schemas"]["Expense"];

/** The api's limits (api/openapi.yaml): each amount at most 100000000 minor units, at most 100 items. */
export const MAX_MINOR = 100_000_000;
export const MAX_ITEMS = 100;
const MAX_DESCRIPTION = 200;

export interface DraftItem {
  /** Local identity for the list (a new item has no api id yet). */
  key: string;
  /** The api's item id, or null for an item added during the review. */
  id: string | null;
  description: string;
  price: string;
}

export interface Draft {
  items: DraftItem[];
  tax: string;
  tip: string;
}

/**
 * "12.50" -> 1250, "12" -> 1200, "12.5" -> 1250, ".5" -> 50. Null for anything else: empty,
 * negative, a third decimal, a comma, letters, an amount over the api's limit.
 */
export function parseMinor(text: string): number | null {
  const match = /^(\d*)(?:\.(\d{1,2}))?$/.exec(text.trim());
  if (match === null || (match[1] === "" && match[2] === undefined)) return null;
  const whole = match[1] === "" ? 0 : Number(match[1]);
  const cents = match[2] === undefined ? 0 : Number(match[2].padEnd(2, "0"));
  const minor = whole * 100 + cents;
  // the limit also refuses a number too big for a float to hold exactly (even Infinity)
  return minor <= MAX_MINOR ? minor : null;
}

/** 1250 -> "12.50": what an amount field shows, with no thousands separator so it parses back. */
export function minorToInput(minor: number): string {
  if (!Number.isSafeInteger(minor) || minor < 0) throw new RangeError(`not an amount: ${minor}`);
  const cents = minor % 100;
  return `${(minor - cents) / 100}.${String(cents).padStart(2, "0")}`;
}

export function draftFrom(expense: Pick<Expense, "items" | "tax_minor" | "tip_minor">): Draft {
  return {
    items: expense.items.map((item) => ({
      key: item.id,
      id: item.id,
      description: item.description,
      price: minorToInput(item.price_minor),
    })),
    tax: minorToInput(expense.tax_minor),
    tip: minorToInput(expense.tip_minor),
  };
}

export type BuiltEdit = { ok: true; body: EditBody | null } | { ok: false; problems: string[] };

/**
 * The PATCH body for what changed between the expense and the draft (null when nothing did, so
 * confirming needs no edit), or what is wrong with the draft. Only changed fields are sent:
 * an item's description and price separately, ids that were removed, items that were added
 * (the api places them after the existing ones), tax and tip when they differ. The `version`
 * is added by the caller.
 */
export function buildEdit(
  expense: Pick<Expense, "items" | "tax_minor" | "tip_minor">,
  draft: Draft,
): BuiltEdit {
  const problems: string[] = [];
  const original = new Map(expense.items.map((item) => [item.id, item]));
  const update: NonNullable<EditBody["update"]> = [];
  const add: NonNullable<EditBody["add"]> = [];

  draft.items.forEach((item, index) => {
    const label = `Item ${index + 1}`;
    const description = item.description.trim();
    const price = parseMinor(item.price);
    if (description === "") problems.push(`${label} needs a name.`);
    else if (description.length > MAX_DESCRIPTION) problems.push(`${label}'s name is too long.`);
    if (price === null) problems.push(`${label} needs a price like 12.50.`);
    if (description === "" || description.length > MAX_DESCRIPTION || price === null) return;

    const before = item.id === null ? undefined : original.get(item.id);
    if (before === undefined) {
      add.push({ description, price_minor: price });
      return;
    }
    const changes: NonNullable<EditBody["update"]>[number] = { item_id: before.id };
    if (description !== before.description) changes.description = description;
    if (price !== before.price_minor) changes.price_minor = price;
    if (changes.description !== undefined || changes.price_minor !== undefined) update.push(changes);
  });

  const tax = parseMinor(draft.tax);
  const tip = parseMinor(draft.tip);
  if (tax === null) problems.push("Tax needs an amount like 3.50 (0 for none).");
  if (tip === null) problems.push("Tip needs an amount like 3.50 (0 for none).");
  if (draft.items.length > MAX_ITEMS) problems.push(`At most ${MAX_ITEMS} items.`);
  if (problems.length > 0 || tax === null || tip === null) return { ok: false, problems };

  const kept = new Set(draft.items.flatMap((item) => (item.id === null ? [] : [item.id])));
  const remove = expense.items.filter((item) => !kept.has(item.id)).map((item) => item.id);

  const body: EditBody = {};
  if (update.length > 0) body.update = update;
  if (remove.length > 0) body.remove = remove;
  if (add.length > 0) body.add = add;
  if (tax !== expense.tax_minor) body.tax_minor = tax;
  if (tip !== expense.tip_minor) body.tip_minor = tip;
  return { ok: true, body: Object.keys(body).length === 0 ? null : body };
}

/** subtotal + tax + tip of what is typed, or null while any of it does not parse. */
export function draftTotal(draft: Draft): number | null {
  const tax = parseMinor(draft.tax);
  const tip = parseMinor(draft.tip);
  if (tax === null || tip === null) return null;
  let total = tax + tip;
  for (const item of draft.items) {
    const price = parseMinor(item.price);
    if (price === null) return null;
    total += price;
  }
  return total;
}
