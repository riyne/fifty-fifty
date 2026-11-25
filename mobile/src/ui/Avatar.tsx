import { StyleSheet, Text, View } from "react-native";

// Soft, similar tints: enough to tell people apart, never louder than the amounts. Dark text on
// a light tint in both appearances.
const TINTS = ["#CFE3DA", "#DAD9EC", "#EFDCCB", "#CFE0EA", "#E9D6DE", "#E2E4C8"];

/** A person's initial on a tint that depends only on their id, so it is the same on every screen. */
export function Avatar({ id, name, size = 36 }: { id: string; name: string; size?: number }) {
  let sum = 0;
  for (const char of id) sum += char.charCodeAt(0);
  return (
    <View
      accessible={false}
      style={[styles.circle, { width: size, height: size, borderRadius: size / 2, backgroundColor: TINTS[sum % TINTS.length] }]}
    >
      <Text allowFontScaling={false} style={[styles.initial, { fontSize: size * 0.42 }]}>
        {(Array.from(name.trim())[0] ?? "?").toUpperCase()}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  circle: { alignItems: "center", justifyContent: "center" },
  initial: { fontWeight: "600", color: "#26322D" },
});
