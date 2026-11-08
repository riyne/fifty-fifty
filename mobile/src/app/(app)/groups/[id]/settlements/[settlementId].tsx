import { useFocusEffect, useLocalSearchParams } from "expo-router";
import { useCallback } from "react";
import { ActivityIndicator, RefreshControl, ScrollView, StyleSheet, Text, View } from "react-native";

import { unwrap } from "../../../../../api/client";
import { api } from "../../../../../api/instance";
import { isOpen, settlementTitle, statusLine } from "../../../../../api/settlements";
import { useLoad } from "../../../../../hooks/useLoad";
import { usePolling } from "../../../../../hooks/usePolling";
import { LoadGate } from "../../../../../ui/LoadGate";

// Where one settlement stands. A settlement is `pending` until the (simulated) provider answers,
// and `unknown` when the answer was unclear: both still reserve the money and can still change,
// so the screen looks again every 2 s for up to a minute; `succeeded` and `failed` are final.

export default function SettlementScreen() {
  const { id, settlementId } = useLocalSearchParams<{ id: string; settlementId: string }>();
  const { state, reload } = useLoad(async () => {
    const [settlement, balances, me] = await Promise.all([
      unwrap(api.GET("/v1/groups/{id}/settlements/{settlementId}", { params: { path: { id, settlementId } } })),
      unwrap(api.GET("/v1/groups/{id}/balances", { params: { path: { id } } })), // the members' names
      unwrap(api.GET("/v1/me")),
    ]);
    return { settlement, balances, me };
  });

  useFocusEffect(
    useCallback(() => {
      void reload();
    }, [reload]),
  );

  const open = state.status === "ready" && isOpen(state.data.settlement.status);
  const { gaveUp, restart } = usePolling(open, () => void reload({ silent: true }), 2000, 60_000);

  return (
    <LoadGate load={state} onRetry={reload}>
      {({ settlement, balances, me }, refreshing) => {
        const names = new Map(balances.balances.map((b) => [b.member_id, b.display_name]));
        const final = !isOpen(settlement.status);
        return (
          <ScrollView
            contentInsetAdjustmentBehavior="automatic"
            contentContainerStyle={styles.container}
            refreshControl={
              <RefreshControl
                refreshing={refreshing}
                onRefresh={() => {
                  restart();
                  void reload();
                }}
              />
            }
          >
            <Text style={styles.title}>
              {settlementTitle(settlement, me.id, (m) => names.get(m) ?? "Someone", settlement.currency)}
            </Text>
            <View style={[styles.banner, settlement.status === "failed" && styles.bannerFailed]}>
              {!final && !gaveUp && <ActivityIndicator />}
              <Text style={styles.bannerText}>
                {!final && gaveUp ? "Still waiting. Pull down to check again." : statusLine(settlement)}
              </Text>
            </View>
            <Text style={styles.note}>Settlement is simulated: a mock payment provider answers, and no real money moves.</Text>
            {settlement.resolved_at !== null && (
              <Text style={styles.note}>Resolved {new Date(settlement.resolved_at).toLocaleString()}</Text>
            )}
          </ScrollView>
        );
      }}
    </LoadGate>
  );
}

const styles = StyleSheet.create({
  container: { padding: 16, gap: 12 },
  title: { fontSize: 22, fontWeight: "700" },
  banner: { flexDirection: "row", alignItems: "center", gap: 12, padding: 12, borderRadius: 8, backgroundColor: "#eef" },
  bannerFailed: { backgroundColor: "#fee" },
  bannerText: { flex: 1 },
  note: { color: "#666", fontSize: 12 },
});
