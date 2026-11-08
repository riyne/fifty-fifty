import { collectPages, MAX_PAGES, type Page } from "./pages";

// A server with `total` items, `size` per page, cursors "c1", "c2", ...
function server(total: number, size: number) {
  const calls: (string | undefined)[] = [];
  const fetchPage = async (cursor: string | undefined): Promise<Page<number>> => {
    calls.push(cursor);
    const start = cursor ? Number(cursor.slice(1)) : 0;
    const items = Array.from({ length: Math.min(size, total - start) }, (_, i) => start + i);
    const end = start + items.length;
    return { items, next: end < total ? `c${end}` : undefined };
  };
  return { fetchPage, calls };
}

describe("collectPages", () => {
  it("returns a single page without a second request", async () => {
    const s = server(3, 100);
    expect(await collectPages(s.fetchPage)).toEqual([0, 1, 2]);
    expect(s.calls).toEqual([undefined]);
  });

  it("follows the cursors and keeps the order", async () => {
    const s = server(7, 3);
    expect(await collectPages(s.fetchPage)).toEqual([0, 1, 2, 3, 4, 5, 6]);
    expect(s.calls).toEqual([undefined, "c3", "c6"]);
  });

  it("handles an exact multiple of the page size and an empty list", async () => {
    expect(await collectPages(server(6, 3).fetchPage)).toEqual([0, 1, 2, 3, 4, 5]);
    expect(await collectPages(server(0, 3).fetchPage)).toEqual([]);
  });

  it("stops at a cursor the server already gave, instead of looping", async () => {
    let calls = 0;
    const loop = async (): Promise<Page<number>> => ({ items: [++calls], next: "same" });
    expect(await collectPages(loop)).toEqual([1, 2]);
    expect(calls).toBe(2);
  });

  it("stops after MAX_PAGES pages of a list that never ends", async () => {
    let calls = 0;
    const endless = async (): Promise<Page<number>> => ({ items: [calls], next: `n${++calls}` });
    expect(await collectPages(endless)).toHaveLength(MAX_PAGES);
    expect(calls).toBe(MAX_PAGES);
  });

  it("lets an error from a page through, so the screen shows its error state", async () => {
    const failing = async (cursor: string | undefined): Promise<Page<number>> => {
      if (cursor) throw new Error("second page failed");
      return { items: [1], next: "c1" };
    };
    await expect(collectPages(failing)).rejects.toThrow("second page failed");
  });
});
