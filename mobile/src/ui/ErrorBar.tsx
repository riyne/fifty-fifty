import { StyleSheet, View } from "react-native";

import { space, useColors } from "./theme";
import { Txt } from "./Txt";

/** What went wrong, placed between the scrolling part and the footer so it is always in view. */
export function ErrorBar({ text }: { text: string }) {
  const colors = useColors();
  return (
    <View style={[styles.bar, { backgroundColor: colors.dangerSoft }]} accessibilityLiveRegion="polite">
      <Txt tone="danger">{text}</Txt>
    </View>
  );
}

const styles = StyleSheet.create({ bar: { paddingHorizontal: space.l, paddingVertical: space.m } });
