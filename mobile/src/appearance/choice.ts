export const APPEARANCES = ["System", "Light", "Dark"] as const;
export type AppearanceChoice = (typeof APPEARANCES)[number];

/** What was stored, or "System" when nothing (or nothing known) was. */
export function parseAppearance(stored: string | null): AppearanceChoice {
  return APPEARANCES.find((choice) => choice === stored) ?? "System";
}

/** What React Native calls each choice; "auto" removes the override, so the app follows the phone. */
export function schemeFor(choice: AppearanceChoice): "auto" | "light" | "dark" {
  return choice === "Light" ? "light" : choice === "Dark" ? "dark" : "auto";
}
