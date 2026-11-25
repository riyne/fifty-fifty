import { APPEARANCES, parseAppearance, schemeFor } from "./choice";

describe("appearance choice", () => {
  it("reads back every choice it stores", () => {
    expect(APPEARANCES.map(parseAppearance)).toEqual(["System", "Light", "Dark"]);
  });

  it("follows the phone when nothing, or something unknown, is stored", () => {
    for (const stored of [null, "", "dark", "Blue"]) expect(parseAppearance(stored)).toBe("System");
  });

  it("maps to the override the platform takes", () => {
    expect(APPEARANCES.map(schemeFor)).toEqual(["auto", "light", "dark"]);
  });
});
