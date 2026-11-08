// The list endpoints return one page at a time (M10.15): a body with the items and, when there
// are more, a `next_cursor` to send back as `cursor`. This follows the cursors so a screen gets
// the whole list, as it did when the server returned everything it had up to 100.

/** What one page of any list endpoint gives, with the item field renamed to `items`. */
export type Page<T> = { items: T[]; next?: string };

/** At most this many pages are fetched (100 items each: 5,000). A server bug that never ends a list must not hang the app. */
export const MAX_PAGES = 50;

/**
 * Fetches pages until there is no next cursor, and returns every item in order. It stops, without an
 * error, after MAX_PAGES pages or if the server hands back a cursor it already gave (a loop); a
 * screen then shows what it has.
 */
export async function collectPages<T>(fetchPage: (cursor: string | undefined) => Promise<Page<T>>): Promise<T[]> {
  const items: T[] = [];
  const seen = new Set<string>();
  let cursor: string | undefined;
  for (let n = 0; n < MAX_PAGES; n++) {
    const page = await fetchPage(cursor);
    items.push(...page.items);
    if (!page.next || seen.has(page.next)) break;
    seen.add(page.next);
    cursor = page.next;
  }
  return items;
}
