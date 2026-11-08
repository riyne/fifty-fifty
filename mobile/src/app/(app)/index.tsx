import { router, useFocusEffect } from "expo-router";
import { useCallback } from "react";
import { Button, FlatList, Pressable, StyleSheet, Text, View } from "react-native";

import { unwrap } from "../../api/client";
import { api } from "../../api/instance";
import { session } from "../../auth";
import { useLoad } from "../../hooks/useLoad";
import { LoadGate } from "../../ui/LoadGate";

export default function Groups() {
  const { state, reload } = useLoad(async () => {
    const [groups, me] = await Promise.all([unwrap(api.GET("/v1/groups")), unwrap(api.GET("/v1/me"))]);
    return { groups: groups.groups, me };
  });

  // Runs when the screen is first shown and every time it comes back into view (after creating
  // or joining a group), so the list is never stale.
  useFocusEffect(
    useCallback(() => {
      void reload();
    }, [reload]),
  );

  return (
    <LoadGate load={state} onRetry={reload}>
      {({ groups, me }, refreshing) => (
        <FlatList
          data={groups}
          contentInsetAdjustmentBehavior="automatic" // start below the translucent iOS header
          keyExtractor={(group) => group.id}
          refreshing={refreshing}
          onRefresh={reload}
          ListHeaderComponent={
            <View style={styles.actions}>
              <Button title="New group" onPress={() => router.push("/groups/new")} />
              <Button title="Join with a code" onPress={() => router.push("/join")} />
            </View>
          }
          ListEmptyComponent={<Text style={styles.empty}>You are not in any group yet. Create one, or join with a code.</Text>}
          renderItem={({ item }) => (
            <Pressable
              style={styles.row}
              onPress={() => router.push({ pathname: "/groups/[id]", params: { id: item.id } })}
            >
              <Text style={styles.name}>{item.name}</Text>
              <Text style={styles.meta}>
                {item.currency} · {item.role}
              </Text>
            </Pressable>
          )}
          ListFooterComponent={
            <View style={styles.footer}>
              <Text style={styles.meta}>Signed in as {me.display_name}</Text>
              <Button title="Sign out" onPress={() => void session.signOut()} />
            </View>
          }
        />
      )}
    </LoadGate>
  );
}

const styles = StyleSheet.create({
  actions: { flexDirection: "row", justifyContent: "space-around", padding: 12 },
  row: { padding: 16, borderBottomWidth: StyleSheet.hairlineWidth, borderBottomColor: "#ccc", gap: 4 },
  name: { fontSize: 17, fontWeight: "600" },
  meta: { color: "#666" },
  empty: { padding: 24, textAlign: "center", color: "#444" },
  footer: { alignItems: "center", padding: 24, gap: 8 },
});
