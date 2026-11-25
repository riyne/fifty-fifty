import type { ReactNode } from "react";
import { StyleSheet, View } from "react-native";
import { useSafeAreaInsets } from "react-native-safe-area-context";

import { space, useColors } from "./theme";

/** The screen's main action, kept at the bottom within reach of a thumb while the rest scrolls. */
export function Footer({ children }: { children: ReactNode }) {
  const colors = useColors();
  const { bottom } = useSafeAreaInsets();
  return (
    <View
      style={[
        styles.bar,
        { backgroundColor: colors.background, borderTopColor: colors.hairline, paddingBottom: Math.max(bottom, space.m) },
      ]}
    >
      {children}
    </View>
  );
}

const styles = StyleSheet.create({
  bar: { flexDirection: "row", gap: space.m, paddingHorizontal: space.l, paddingTop: space.m, borderTopWidth: StyleSheet.hairlineWidth },
});
