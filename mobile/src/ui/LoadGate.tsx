import type { ReactNode } from "react";
import { ActivityIndicator, StyleSheet, View } from "react-native";

import type { Load } from "../hooks/useLoad";
import { Button } from "./Button";
import { EmptyState } from "./EmptyState";

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
        <EmptyState icon="exclamationmark.triangle" title="That didn't load" hint={load.message} />
        <Button title="Try again" onPress={onRetry} />
      </View>
    );
  }
  return <>{children(load.data, load.refreshing)}</>;
}

const styles = StyleSheet.create({
  center: { flex: 1, justifyContent: "center", padding: 24 },
});
