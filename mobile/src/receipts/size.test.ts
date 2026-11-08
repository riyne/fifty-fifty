import { fitWithin, MAX_SIDE_PX } from "./size";

describe("fitWithin", () => {
  it("leaves an image that already fits alone", () => {
    expect(fitWithin(2400, 1800)).toBeNull();
    expect(fitWithin(1000, 3000, 3000)).toBeNull();
    expect(fitWithin(100, 100)).toBeNull();
  });

  it("scales the longer side down to the limit and keeps the aspect ratio", () => {
    expect(fitWithin(4032, 3024)).toEqual({ width: 2400, height: 1800 }); // a 12 MP phone photo
    expect(fitWithin(3024, 4032)).toEqual({ width: 1800, height: 2400 }); // the same, upright
    expect(fitWithin(8000, 8000)).toEqual({ width: 2400, height: 2400 });
  });

  it("brings a 48 MP photo under the api's 25 million pixel limit", () => {
    const size = fitWithin(8064, 6048);
    expect(size).toEqual({ width: 2400, height: 1800 });
    expect(size!.width * size!.height).toBeLessThan(25_000_000);
  });

  it("keeps a very thin image at least one pixel on the short side", () => {
    expect(fitWithin(100_000, 1)).toEqual({ width: MAX_SIDE_PX, height: 1 });
  });

  it("rounds to whole pixels", () => {
    const size = fitWithin(5000, 3333)!;
    expect(size.width).toBe(2400);
    expect(Number.isInteger(size.height)).toBe(true);
    expect(size.height).toBe(1600); // 3333 * 2400 / 5000 = 1599.84
  });

  it("refuses sizes that are not positive integers", () => {
    expect(() => fitWithin(0, 100)).toThrow(RangeError);
    expect(() => fitWithin(100.5, 100)).toThrow(RangeError);
    expect(() => fitWithin(100, 100, 0)).toThrow(RangeError);
    expect(() => fitWithin(NaN, 100)).toThrow(RangeError);
  });
});
