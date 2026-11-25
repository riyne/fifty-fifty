import { Pressable, StyleSheet, View } from "react-native";

import { Icon } from "./Icon";
import { radius, space, useColors } from "./theme";
import { Txt } from "./Txt";

/** Pick one of a few short options. */
export function Choice<T extends string>({
  label,
  options,
  value,
  onChange,
  disabled = false,
}: {
  label: string;
  options: readonly T[];
  value: T;
  onChange: (value: T) => void;
  disabled?: boolean;
}) {
  const colors = useColors();
  return (
    <View style={styles.box}>
      <Txt variant="small" tone="secondary" style={styles.label}>
        {label}
      </Txt>
      <View style={styles.options} accessibilityRole="radiogroup">
        {options.map((option) => {
          const selected = option === value;
          return (
            <Pressable
              key={option}
              accessibilityRole="radio"
              accessibilityLabel={option}
              accessibilityState={{ selected, disabled }}
              disabled={disabled}
              onPress={() => onChange(option)}
              style={[
                styles.option,
                { backgroundColor: selected ? colors.accentSoft : colors.card, borderColor: selected ? colors.accent : colors.hairline },
              ]}
            >
              {selected && <Icon name="checkmark" size={14} tone="accent" />}
              <Txt variant="heading" tone={selected ? "accent" : "text"}>
                {option}
              </Txt>
            </Pressable>
          );
        })}
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  box: { gap: space.xs },
  label: { paddingHorizontal: space.xs },
  options: { flexDirection: "row", gap: space.m },
  option: { flex: 1, minHeight: 48, flexDirection: "row", gap: space.s, alignItems: "center", justifyContent: "center", borderWidth: 1, borderRadius: radius.s },
});
