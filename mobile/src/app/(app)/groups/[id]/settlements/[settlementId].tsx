import { useLocalSearchParams } from "expo-router";
import { ActivityIndicator, RefreshControl, ScrollView, StyleSheet, View } from "react-native";

import { unwrap } from "../../../../../api/client";
import { api } from "../../../../../api/instance";
import { isOpen, settlementTitle, statusLine, statusWord } from "../../../../../api/settlements";
import { useLoad } from "../../../../../hooks/useLoad";
import { useReloadOnReturn } from "../../../../../hooks/useReloadOnReturn";
import { usePolling } from "../../../../../hooks/usePolling";
import { Icon } from "../../../../../ui/Icon";
import { LoadGate } from "../../../../../ui/LoadGate";
import { Card } from "../../../../../ui/Section";
import { space } from "../../../../../ui/theme";
import { Txt } from "../../../../../ui/Txt";

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

  useReloadOnReturn(reload);

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
            <Card padded>
              <View style={styles.status}>
                {final ? (
                  <Icon
                    name={settlement.status === "succeeded" ? "checkmark.circle.fill" : "xmark.circle.fill"}
                    size={56}
                    tone={settlement.status === "succeeded" ? "accent" : "danger"}
                  />
                ) : gaveUp ? (
                  <Icon name="clock" size={56} tone="owe" />
                ) : (
                  <ActivityIndicator size="large" />
                )}
                <Txt variant="title">{statusWord(settlement.status)}</Txt>
                <Txt variant="heading" style={styles.centered}>
                  {settlementTitle(settlement, me.id, (m) => names.get(m) ?? "Someone", settlement.currency)}
                </Txt>
                <Txt tone="secondary" style={styles.centered}>
                  {!final && gaveUp ? "Still waiting. Pull down to check again." : statusLine(settlement)}
                </Txt>
              </View>
            </Card>
            <Txt variant="small" tone="secondary" style={styles.centered}>
              Simulated: a mock payment provider answers, and no real money moves.
            </Txt>
            {settlement.resolved_at !== null && (
              <Txt variant="small" tone="secondary" style={styles.centered}>
                Resolved {new Date(settlement.resolved_at).toLocaleString()}
              </Txt>
            )}
          </ScrollView>
        );
      }}
    </LoadGate>
  );
}

const styles = StyleSheet.create({
  container: { padding: space.l, gap: space.m },
  status: { alignItems: "center", gap: space.m, paddingVertical: space.l },
  centered: { textAlign: "center" },
});
