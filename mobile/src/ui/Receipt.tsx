import type { ReactNode } from "react";
import { Pressable, StyleSheet, useWindowDimensions, View } from "react-native";

import { space, useColors } from "./theme";
import { Txt } from "./Txt";

const TOOTH = 12;

/** A paper slip with torn edges. The teeth are squares turned 45 degrees and cut in half (no SVG). */
export function Receipt({ children }: { children: ReactNode }) {
  const colors = useColors();
  return (
    <View>
      <TornEdge side="top" />
      <View style={[styles.paper, { backgroundColor: colors.card }]}>{children}</View>
      <TornEdge side="bottom" />
    </View>
  );
}

function TornEdge({ side }: { side: "top" | "bottom" }) {
  const colors = useColors();
  const { width } = useWindowDimensions();
  // enough teeth for the whole screen; the slip is narrower, and the rest is cut off
  const teeth = Math.ceil(width / TOOTH);
  return (
    <View style={styles.edge} accessible={false}>
      {Array.from({ length: teeth }, (_, index) => (
        <View
          key={index}
          style={[
            styles.tooth,
            { left: index * TOOTH, backgroundColor: colors.card },
            side === "top" ? styles.toothTop : styles.toothBottom,
          ]}
        />
      ))}
    </View>
  );
}

/** The dashed line between the parts of a receipt. */
export function Dashes() {
  const colors = useColors();
  return (
    <View style={styles.dashes} accessible={false}>
      <View style={[styles.dashLine, { borderColor: colors.secondary }]} />
    </View>
  );
}

/** An amount in receipt type, so a column of prices lines up. */
export function Price({ text, strong = false }: { text: string; strong?: boolean }) {
  return (
    <Txt variant={strong ? "strong" : "body"} style={styles.mono}>
      {text}
    </Txt>
  );
}

/** "Tax ........ 14.20" */
export function TotalLine({ label, amount, strong = false }: { label: string; amount: string; strong?: boolean }) {
  return (
    <View style={styles.totalLine} accessible>
      <Txt variant={strong ? "strong" : "body"} tone={strong ? "text" : "secondary"} style={styles.mono}>
        {label}
      </Txt>
      <Price text={amount} strong={strong} />
    </View>
  );
}

/** One item. With `onPress` the whole line is the target, and it is a checkbox to VoiceOver. */
export function ReceiptLine({
  mark,
  title,
  detail,
  price,
  accessibilityLabel,
  checked,
  onPress,
}: {
  mark?: ReactNode;
  title: string;
  detail?: ReactNode;
  price: string;
  accessibilityLabel: string;
  checked: boolean;
  onPress?: () => void;
}) {
  const colors = useColors();
  const content = (
    <>
      {mark}
      <View style={styles.lineText}>
        <Txt variant="strong">{title}</Txt>
        {detail}
      </View>
      <Price text={price} />
    </>
  );
  if (onPress === undefined) {
    return (
      <View accessible accessibilityLabel={accessibilityLabel} style={styles.line}>
        {content}
      </View>
    );
  }
  return (
    <Pressable
      accessibilityRole="checkbox"
      accessibilityState={{ checked }}
      accessibilityLabel={accessibilityLabel}
      onPress={onPress}
      style={({ pressed }) => [styles.line, pressed && { backgroundColor: colors.pressed }]}
    >
      {content}
    </Pressable>
  );
}

const styles = StyleSheet.create({
  paper: { paddingVertical: space.m },
  edge: { height: TOOTH / 2, overflow: "hidden" },
  // a square whose diagonal is one tooth wide, centred on the paper's edge
  tooth: { position: "absolute", width: TOOTH * Math.SQRT1_2, height: TOOTH * Math.SQRT1_2, marginLeft: (TOOTH * (1 - Math.SQRT1_2)) / 2, transform: [{ rotate: "45deg" }] },
  toothTop: { top: TOOTH / 2 - (TOOTH * Math.SQRT1_2) / 2 },
  toothBottom: { top: -(TOOTH * Math.SQRT1_2) / 2 },
  // a dashed border is drawn on all four sides only, so one side is shown and the rest cut off
  dashes: { height: 1, overflow: "hidden", marginHorizontal: space.l, marginVertical: space.m, opacity: 0.5 },
  dashLine: { height: 2, borderWidth: 1, borderStyle: "dashed" },
  mono: { fontFamily: "ui-monospace", fontSize: 15 },
  totalLine: { flexDirection: "row", justifyContent: "space-between", gap: space.m, paddingHorizontal: space.l, paddingVertical: 2 },
  line: { flexDirection: "row", alignItems: "center", gap: space.m, paddingHorizontal: space.l, paddingVertical: space.s, minHeight: 56 },
  lineText: { flex: 1, gap: 3 },
});
