import { buildEdit, draftFrom, draftTotal, minorToInput, parseMinor, type Draft } from "./review";

const item = (id: string, description: string, price_minor: number) => ({ id, position: 0, description, price_minor, claimed_by: [] });
const expense = {
  items: [item("a", "Pasta", 2000), item("b", "Wine", 1500), item("c", "Salad", 900)],
  tax_minor: 220,
  tip_minor: 330,
};
const unchanged = (): Draft => draftFrom(expense);

describe("parseMinor", () => {
  it("reads whole and decimal amounts without a float", () => {
    expect(parseMinor("12.50")).toBe(1250);
    expect(parseMinor("12")).toBe(1200);
    expect(parseMinor("12.5")).toBe(1250);
    expect(parseMinor(".5")).toBe(50);
    expect(parseMinor("0")).toBe(0);
    expect(parseMinor("0.07")).toBe(7);
    expect(parseMinor(" 3.10 ")).toBe(310);
    expect(parseMinor("19.99")).toBe(1999); // 19.99 * 100 is 1998.9999999999998 as a float
    expect(parseMinor("1.15")).toBe(115); // 1.15 * 100 is 114.99999999999999 as a float
  });

  it("refuses what is not an amount", () => {
    for (const text of ["", " ", ".", "-1", "-0.50", "1.234", "1,50", "1,000.00", "12.", "abc", "1e3", "12 50", "$12", "+5"]) {
      expect(parseMinor(text)).toBeNull();
    }
  });

  it("stops at the api's limit of 100000000 minor units", () => {
    expect(parseMinor("1000000.00")).toBe(100_000_000);
    expect(parseMinor("1000000.01")).toBeNull();
    expect(parseMinor("99999999999999999999")).toBeNull();
    expect(parseMinor("9".repeat(400))).toBeNull(); // a float holds this as Infinity
  });
});

describe("minorToInput", () => {
  it("writes an amount the field can parse back", () => {
    for (const minor of [0, 7, 50, 1250, 123456, 100_000_000]) expect(parseMinor(minorToInput(minor))).toBe(minor);
    expect(minorToInput(1250)).toBe("12.50");
    expect(minorToInput(7)).toBe("0.07");
    expect(minorToInput(123456)).toBe("1234.56"); // no thousands separator
  });
  it("refuses a negative or fractional amount", () => {
    expect(() => minorToInput(-1)).toThrow(RangeError);
    expect(() => minorToInput(1.5)).toThrow(RangeError);
  });
});

describe("buildEdit", () => {
  it("is null when nothing changed, so confirming sends no edit", () => {
    expect(buildEdit(expense, unchanged())).toEqual({ ok: true, body: null });
  });

  it("is null when text was retyped to the same values", () => {
    const draft = unchanged();
    draft.items[0].price = "20"; // was "20.00"
    draft.items[1].description = "  Wine ";
    expect(buildEdit(expense, draft)).toEqual({ ok: true, body: null });
  });

  it("sends only the fields that changed", () => {
    const draft = unchanged();
    draft.items[0].price = "21.00";
    draft.items[1].description = "Red wine";
    draft.tip = "4.00";
    expect(buildEdit(expense, draft)).toEqual({
      ok: true,
      body: {
        update: [
          { item_id: "a", price_minor: 2100 },
          { item_id: "b", description: "Red wine" },
        ],
        tip_minor: 400,
      },
    });
  });

  it("removes the items that are gone and adds the new ones in order", () => {
    const draft = unchanged();
    draft.items = [draft.items[0], draft.items[2]];
    draft.items.push({ key: "n1", id: null, description: " Tea ", price: "2.5" }, { key: "n2", id: null, description: "Cake", price: "6" });
    expect(buildEdit(expense, draft)).toEqual({
      ok: true,
      body: {
        remove: ["b"],
        add: [
          { description: "Tea", price_minor: 250 },
          { description: "Cake", price_minor: 600 },
        ],
      },
    });
  });

  it("can set tax or tip to zero", () => {
    const draft = unchanged();
    draft.tax = "0";
    expect(buildEdit(expense, draft)).toEqual({ ok: true, body: { tax_minor: 0 } });
  });

  it("can empty the list: the api allows an empty draft and confirm refuses it", () => {
    const draft = unchanged();
    draft.items = [];
    expect(buildEdit(expense, draft)).toEqual({ ok: true, body: { remove: ["a", "b", "c"] } });
  });

  it("names every problem instead of sending a bad request", () => {
    const draft = unchanged();
    draft.items[0].description = "   ";
    draft.items[1].price = "12,50";
    draft.items.push({ key: "n", id: null, description: "x".repeat(201), price: "1" });
    draft.tax = "";
    draft.tip = "-1";
    const built = buildEdit(expense, draft);
    expect(built.ok).toBe(false);
    if (!built.ok) {
      expect(built.problems).toEqual([
        "Item 1 needs a name.",
        "Item 2 needs a price like 12.50.",
        "Item 4's name is too long.",
        "Tax needs an amount like 3.50 (0 for none).",
        "Tip needs an amount like 3.50 (0 for none).",
      ]);
    }
  });

  it("accepts a name of exactly 200 characters and refuses 201", () => {
    const draft = unchanged();
    draft.items[0].description = "x".repeat(200);
    expect(buildEdit(expense, draft).ok).toBe(true);
    draft.items[0].description = "x".repeat(201);
    expect(buildEdit(expense, draft).ok).toBe(false);
  });

  it("refuses more than 100 items", () => {
    const draft = unchanged();
    for (let i = 0; i < 98; i++) draft.items.push({ key: `n${i}`, id: null, description: "x", price: "1" });
    expect(buildEdit(expense, draft).ok).toBe(false); // 101 items
    draft.items.pop();
    expect(buildEdit(expense, draft).ok).toBe(true); // 100 items
  });
});

describe("draftTotal", () => {
  it("adds the items, tax and tip in minor units", () => {
    expect(draftTotal(unchanged())).toBe(2000 + 1500 + 900 + 220 + 330);
    const draft = unchanged();
    draft.items.push({ key: "n", id: null, description: "Tea", price: "0.10" });
    draft.items.push({ key: "m", id: null, description: "Tea", price: "0.20" });
    expect(draftTotal(draft)).toBe(4950 + 30); // 0.1 + 0.2 is not 0.3 as floats; here it is exact
  });

  it("is null while any amount does not parse", () => {
    const draft = unchanged();
    draft.items[0].price = "2o.00";
    expect(draftTotal(draft)).toBeNull();
    const other = unchanged();
    other.tip = "";
    expect(draftTotal(other)).toBeNull();
  });
});
