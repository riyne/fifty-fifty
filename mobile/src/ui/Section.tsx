import { Children, Fragment, type ReactNode } from "react";
import { Pressable, StyleSheet, View } from "react-native";

import { Icon } from "./Icon";
import { radius, space, useColors } from "./theme";
import { Txt } from "./Txt";

/** A rounded card. */
export function Card({ children, padded = false }: { children: ReactNode; padded?: boolean }) {
  const colors = useColors();
  return <View style={[styles.card, { backgroundColor: colors.card }, padded && styles.padded]}>{children}</View>;
}

/** A heading over a card of rows, with a hairline between the rows, and an optional note under it. */
export function Section({ title, note, children }: { title: string; note?: string; children: ReactNode }) {
  const colors = useColors();
  return (
    <View style={styles.section}>
      <Txt variant="heading" accessibilityRole="header" style={styles.inset}>
        {title}
      </Txt>
      <Card>
        {Children.toArray(children).map((row, index) => (
          <Fragment key={index}>
            {index > 0 && <View style={[styles.line, { backgroundColor: colors.hairline }]} />}
            {row}
          </Fragment>
        ))}
      </Card>
      {note !== undefined && (
        <Txt variant="small" tone="secondary" style={styles.inset}>
          {note}
        </Txt>
      )}
    </View>
  );
}

/** One line of a card. With `onPress` the whole row is the target and shows a chevron. */
export function Row({
  title,
  subtitle,
  leading,
  trailing,
  onPress,
}: {
  title: string;
  subtitle?: string;
  leading?: ReactNode;
  trailing?: ReactNode;
  onPress?: () => void;
}) {
  const colors = useColors();
  const content = (
    <>
      {leading}
      <View style={styles.text}>
        <Txt variant="strong" numberOfLines={2}>
          {title}
        </Txt>
        {subtitle !== undefined && (
          <Txt variant="small" tone="secondary" numberOfLines={2}>
            {subtitle}
          </Txt>
        )}
      </View>
      {trailing !== undefined && <View style={styles.trailing}>{trailing}</View>}
    </>
  );
  if (onPress === undefined) return <View style={styles.row}>{content}</View>;
  return (
    <Pressable
      accessibilityRole="button"
      onPress={onPress}
      style={({ pressed }) => [styles.row, pressed && { backgroundColor: colors.pressed }]}
    >
      {content}
      <Icon name="chevron.right" size={13} />
    </Pressable>
  );
}

const styles = StyleSheet.create({
  card: { borderRadius: radius.m, overflow: "hidden" },
  padded: { padding: space.l, gap: space.m },
  section: { gap: space.s },
  inset: { paddingHorizontal: space.l },
  line: { height: StyleSheet.hairlineWidth, marginLeft: space.l },
  row: { flexDirection: "row", alignItems: "center", gap: space.m, paddingHorizontal: space.l, paddingVertical: space.m, minHeight: 60 },
  text: { flex: 1, gap: 2 },
  trailing: { alignItems: "flex-end", gap: space.xs, maxWidth: "50%" },
});
