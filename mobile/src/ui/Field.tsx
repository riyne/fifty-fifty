import { StyleSheet, TextInput, View, type TextInputProps } from "react-native";

import { radius, space, useColors } from "./theme";
import { Txt } from "./Txt";

/** A text input in the app's look. `label` is shown above it and read out with it. */
export function Field({ label, style, ...rest }: TextInputProps & { label?: string }) {
  const colors = useColors();
  const input = (
    <TextInput
      accessibilityLabel={label}
      placeholderTextColor={colors.secondary}
      selectionColor={colors.accent}
      {...rest}
      style={[styles.input, { backgroundColor: colors.card, borderColor: colors.hairline, color: colors.text }, style]}
    />
  );
  if (label === undefined) return input;
  return (
    <View style={styles.labelled}>
      <Txt variant="small" tone="secondary" style={styles.label}>
        {label}
      </Txt>
      {input}
    </View>
  );
}

const styles = StyleSheet.create({
  labelled: { gap: space.xs },
  label: { paddingHorizontal: space.xs },
  input: { minHeight: 48, borderWidth: 1, borderRadius: radius.s, paddingHorizontal: space.m, fontSize: 16 },
});
