import { ActivityIndicator, Pressable, StyleSheet, View } from "react-native";

import { Icon } from "./Icon";
import { radius, space, useColors, type Colors } from "./theme";
import { Txt } from "./Txt";

type Kind = "primary" | "secondary" | "danger";

/**
 * A full-width button. While `busy` the spinner takes the label's place inside the same box, so
 * nothing on the screen moves, and it can not be pressed again.
 */
export function Button({
  title,
  onPress,
  kind = "primary",
  icon,
  busy = false,
  disabled = false,
  grow = false,
}: {
  title: string;
  onPress: () => void;
  kind?: Kind;
  icon?: Parameters<typeof Icon>[0]["name"];
  busy?: boolean;
  disabled?: boolean;
  /** Share the row with other buttons. */
  grow?: boolean;
}) {
  const colors = useColors();
  const look = {
    primary: { background: "accent", label: "onAccent" },
    secondary: { background: "accentSoft", label: "accent" },
    danger: { background: "dangerSoft", label: "danger" },
  } as const satisfies Record<Kind, { background: keyof Colors; label: keyof Colors }>;
  const { background, label } = look[kind];
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={title}
      accessibilityState={{ disabled: disabled || busy, busy }}
      disabled={disabled || busy}
      onPress={onPress}
      style={({ pressed }) => [
        styles.box,
        grow && styles.grow,
        { backgroundColor: colors[background] },
        pressed && styles.pressed,
        disabled && !busy && styles.disabled,
      ]}
    >
      <View style={[styles.content, busy && styles.hidden]}>
        {icon !== undefined && <Icon name={icon} size={18} tone={label} />}
        <Txt variant="heading" tone={label} numberOfLines={1} adjustsFontSizeToFit style={styles.label}>
          {title}
        </Txt>
      </View>
      {busy && <ActivityIndicator style={StyleSheet.absoluteFill} color={colors[label]} />}
    </Pressable>
  );
}

const styles = StyleSheet.create({
  box: { minHeight: 50, borderRadius: radius.m, paddingHorizontal: space.l, justifyContent: "center" },
  grow: { flex: 1 },
  content: { flexDirection: "row", alignItems: "center", justifyContent: "center", gap: space.s },
  label: { flexShrink: 1 },
  pressed: { opacity: 0.75, transform: [{ scale: 0.985 }] },
  disabled: { opacity: 0.4 },
  hidden: { opacity: 0 },
});
