import { StyleSheet, View } from "react-native";

import { space, useColors, type Colors } from "./theme";
import { Txt } from "./Txt";

export type PillTone = "active" | "attention" | "quiet" | "bad";

/** A short status word. Colour repeats what the word says; it never carries the meaning alone. */
export function Pill({ text, tone }: { text: string; tone: PillTone }) {
  const colors = useColors();
  const look = {
    active: { background: "accentSoft", label: "accent" },
    attention: { background: "oweSoft", label: "owe" },
    quiet: { background: "pressed", label: "secondary" },
    bad: { background: "dangerSoft", label: "danger" },
  } as const satisfies Record<PillTone, { background: keyof Colors; label: keyof Colors }>;
  const { background, label } = look[tone];
  return (
    <View style={[styles.pill, { backgroundColor: colors[background] }]}>
      <Txt variant="small" tone={label} style={styles.text}>
        {text}
      </Txt>
    </View>
  );
}

const styles = StyleSheet.create({
  pill: { paddingHorizontal: space.s, paddingVertical: 1, borderRadius: 20 },
  text: { fontWeight: "600" },
});
