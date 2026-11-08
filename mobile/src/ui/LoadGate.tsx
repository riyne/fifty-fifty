import type { ReactNode } from "react";
import { ActivityIndicator, Button, StyleSheet, Text, View } from "react-native";

import type { Load } from "../hooks/useLoad";

/** Shows a spinner while loading, the message and a retry button on failure, else `children(data)`. */
export function LoadGate<T>({
  load,
  onRetry,
  children,
}: {
  load: Load<T>;
  onRetry: () => void;
  children: (data: T, refreshing: boolean) => ReactNode;
}) {
  if (load.status === "loading") {
    return (
      <View style={styles.center}>
        <ActivityIndicator />
      </View>
    );
  }
  if (load.status === "failed") {
    return (
      <View style={styles.center}>
        <Text style={styles.error}>{load.message}</Text>
        <Button title="Try again" onPress={onRetry} />
      </View>
    );
  }
  return <>{children(load.data, load.refreshing)}</>;
}

const styles = StyleSheet.create({
  center: { flex: 1, alignItems: "center", justifyContent: "center", gap: 12, padding: 24 },
  error: { color: "#b00020", textAlign: "center" },
});
