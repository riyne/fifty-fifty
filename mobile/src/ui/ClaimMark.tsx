import { StyleSheet, View } from "react-native";

import { Icon } from "./Icon";
import { useColors } from "./theme";

/**
 * A circle filled by one person's share of an item: empty when it is not theirs, whole when it is
 * theirs alone, half when two people share it. Drawn with plain views (no SVG): each half of the
 * circle is a window on a half disc that turns behind it.
 */
export function ClaimMark({ share, size = 28 }: { share: number; size?: number }) {
  const colors = useColors();
  const ring = Math.max(1.5, size / 16);
  const inner = size - ring; // reaches half way under the ring, so no seam shows between them
  const half = inner / 2;
  const degrees = Math.max(0, Math.min(1, share)) * 360;
  const disc = { width: inner, height: inner, borderRadius: half, backgroundColor: colors.accent };

  return (
    <View accessible={false} style={{ width: size, height: size, padding: ring / 2 }}>
      {degrees >= 360 ? (
        <View style={[disc, styles.center]}>
          <Icon name="checkmark" size={size * 0.5} tone="onAccent" />
        </View>
      ) : (
        <View style={{ width: inner, height: inner }}>
          {/* right half: shows the first 180 degrees */}
          <View style={[styles.window, { left: half, width: half, height: inner }]}>
            <View style={{ width: inner, height: inner, marginLeft: -half, transform: [{ rotate: `${Math.min(degrees, 180) - 180}deg` }] }}>
              <View style={[styles.window, { left: half, width: half, height: inner }]}>
                <View style={[disc, { marginLeft: -half }]} />
              </View>
            </View>
          </View>
          {/* left half: the rest */}
          {degrees > 180 && (
            <View style={[styles.window, { left: 0, width: half, height: inner }]}>
              <View style={{ width: inner, height: inner, transform: [{ rotate: `${degrees - 360}deg` }] }}>
                <View style={[styles.window, { left: 0, width: half, height: inner }]}>
                  <View style={disc} />
                </View>
              </View>
            </View>
          )}
        </View>
      )}
      <View
        style={[
          StyleSheet.absoluteFill,
          { borderRadius: size / 2, borderWidth: ring, borderColor: degrees > 0 ? colors.accent : colors.secondary },
        ]}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  window: { position: "absolute", top: 0, overflow: "hidden" },
  center: { alignItems: "center", justifyContent: "center" },
});
