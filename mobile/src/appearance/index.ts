import { File, Paths } from "expo-file-system";
import { Appearance } from "react-native";

import { parseAppearance, schemeFor, type AppearanceChoice } from "./choice";

// The person's light / dark choice, in a small file so it is read before the first frame (no flash
// of the other theme). A file that can not be read or written only means the choice is not
// remembered, so nothing is shown for it.

const file = () => new File(Paths.document, "appearance.txt");

export function storedAppearance(): AppearanceChoice {
  try {
    const f = file();
    return parseAppearance(f.exists ? f.textSync() : null);
  } catch {
    return "System";
  }
}

export function chooseAppearance(choice: AppearanceChoice): void {
  Appearance.setColorScheme(schemeFor(choice));
  try {
    const f = file();
    if (!f.exists) f.create();
    f.writeSync(choice);
  } catch {
    // not remembered for the next launch
  }
}

/** Applies the stored choice at launch. */
export function applyStoredAppearance(): void {
  const choice = storedAppearance();
  if (choice !== "System") Appearance.setColorScheme(schemeFor(choice));
}
