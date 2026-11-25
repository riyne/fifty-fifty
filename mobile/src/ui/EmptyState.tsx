import { StyleSheet, View } from "react-native";

import { Icon } from "./Icon";
import { space } from "./theme";
import { Txt } from "./Txt";

/** What a list shows before it has anything: what is missing and what to do about it. */
export function EmptyState({ icon, title, hint }: { icon: Parameters<typeof Icon>[0]["name"]; title: string; hint: string }) {
  return (
    <View style={styles.box}>
      <Icon name={icon} size={34} />
      <Txt variant="heading" style={styles.text}>
        {title}
      </Txt>
      <Txt tone="secondary" style={styles.text}>
        {hint}
      </Txt>
    </View>
  );
}

const styles = StyleSheet.create({
  box: { alignItems: "center", gap: space.s, padding: space.xl },
  text: { textAlign: "center" },
});
