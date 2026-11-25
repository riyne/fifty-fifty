import { SymbolView, type SymbolViewProps } from "expo-symbols";

import { useColors, type Colors } from "./theme";

/** An SF Symbol. Decorative: the text or the label of the control next to it says what it means. */
export function Icon({
  name,
  size = 20,
  tone = "secondary",
}: {
  name: Extract<SymbolViewProps["name"], string>;
  size?: number;
  tone?: keyof Colors;
}) {
  const colors = useColors();
  return <SymbolView name={name} size={size} tintColor={colors[tone]} weight="medium" accessible={false} />;
}
