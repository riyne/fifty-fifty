import { StyleSheet, Text, type TextProps } from "react-native";

import { useColors, type Colors } from "./theme";

type Variant = "display" | "title" | "heading" | "body" | "strong" | "small";
type Tone = Extract<keyof Colors, "text" | "secondary" | "accent" | "onAccent" | "owe" | "danger">;

/** All text in the app, so sizes and colours come from one place. Follows the phone's text size. */
export function Txt({ variant = "body", tone = "text", style, ...rest }: TextProps & { variant?: Variant; tone?: Tone }) {
  const colors = useColors();
  return <Text {...rest} style={[styles[variant], { color: colors[tone] }, style]} />;
}

const styles = StyleSheet.create({
  // tabular figures: an amount does not shift sideways when its digits change
  display: { fontSize: 40, fontWeight: "700", letterSpacing: -0.8, fontVariant: ["tabular-nums"] },
  title: { fontSize: 22, fontWeight: "700", letterSpacing: -0.3 },
  heading: { fontSize: 17, fontWeight: "600" },
  body: { fontSize: 16, lineHeight: 22, fontVariant: ["tabular-nums"] },
  strong: { fontSize: 16, lineHeight: 22, fontWeight: "600", fontVariant: ["tabular-nums"] },
  small: { fontSize: 13, lineHeight: 18, fontVariant: ["tabular-nums"] },
});
