import { router, Stack, useFocusEffect, useLocalSearchParams } from "expo-router";
import { useCallback, useState } from "react";
import { ActivityIndicator, Button, Pressable, RefreshControl, ScrollView, StyleSheet, Text, View } from "react-native";

import { unwrap } from "../../../../api/client";
import { describeError } from "../../../../api/errors";
import { api } from "../../../../api/instance";
import { statusLabel } from "../../../../api/expenses";
import { formatMoney } from "../../../../api/money";
import { balanceSentence, transferSentence } from "../../../../api/sentences";
import type { CreatedJoinCode } from "../../../../api/types";
import { useLoad } from "../../../../hooks/useLoad";
import { LoadGate } from "../../../../ui/LoadGate";

export default function GroupScreen() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const { state, reload } = useLoad(async () => {
    const path = { params: { path: { id } } };
    // one request each; balances and members come from different statements, so a member who
    // joined between the two is simply missing from one list until the next refresh
    const [group, balances, expenses, me] = await Promise.all([
      unwrap(api.GET("/v1/groups/{id}", path)),
      unwrap(api.GET("/v1/groups/{id}/balances", path)),
      unwrap(api.GET("/v1/groups/{id}/expenses", path)),
      unwrap(api.GET("/v1/me")),
    ]);
    return { group, balances, expenses: expenses.expenses, me };
  });

  useFocusEffect(
    useCallback(() => {
      void reload();
    }, [reload]),
  );

  return (
    <LoadGate load={state} onRetry={reload}>
      {({ group, balances, expenses, me }, refreshing) => {
        const names = new Map(balances.balances.map((b) => [b.member_id, b.display_name]));
        const roles = new Map(group.members.map((m) => [m.user_id, m.role]));
        const who = (memberId: string) => ({ name: names.get(memberId) ?? "Someone", isMe: memberId === me.id });
        return (
          <ScrollView
            contentInsetAdjustmentBehavior="automatic"
            contentContainerStyle={styles.container}
            refreshControl={<RefreshControl refreshing={refreshing} onRefresh={reload} />}
          >
            <Stack.Screen options={{ title: group.name }} />
            <Text style={styles.meta}>
              {group.currency} · you are {group.role === "owner" ? "the owner" : "a member"}
            </Text>

            <Text style={styles.heading}>Members and balances</Text>
            {balances.balances.map((b) => (
              <View key={b.member_id} style={styles.row}>
                <Text style={styles.name}>
                  {b.member_id === me.id ? `${b.display_name} (you)` : b.display_name}
                  {roles.get(b.member_id) === "owner" ? " · owner" : ""}
                </Text>
                <Text>{balanceSentence(b.display_name, b.member_id === me.id, b.balance_minor, balances.currency)}</Text>
              </View>
            ))}

            <Text style={styles.heading}>Expenses</Text>
            {expenses.length === 0 ? (
              <Text style={styles.meta}>No expenses yet.</Text>
            ) : (
              expenses.map((e) => (
                <Pressable
                  key={e.id}
                  style={styles.row}
                  onPress={() =>
                    router.push({ pathname: "/groups/[id]/expenses/[expenseId]", params: { id: group.id, expenseId: e.id } })
                  }
                >
                  <Text style={styles.name}>
                    {e.description} · {formatMoney(e.total_minor, e.currency)}
                  </Text>
                  <Text style={styles.meta}>
                    {statusLabel(e.status)} · paid by {who(e.payer_id).isMe ? "you" : who(e.payer_id).name}
                  </Text>
                </Pressable>
              ))
            )}

            <Text style={styles.heading}>Suggested transfers</Text>
            {balances.suggested_transfers.length === 0 ? (
              <Text style={styles.meta}>Nobody owes anybody.</Text>
            ) : (
              balances.suggested_transfers.map((t) => (
                <Text key={`${t.from_member_id}-${t.to_member_id}`}>
                  {transferSentence(who(t.from_member_id), who(t.to_member_id), t.amount_minor, balances.currency)}
                </Text>
              ))
            )}
            <Text style={styles.note}>
              Suggestions only: nothing is recorded until a settlement is made. The list reduces the number of
              payments but is not always the smallest possible.
            </Text>

            {group.role === "owner" && <Invite groupId={group.id} />}
          </ScrollView>
        );
      }}
    </LoadGate>
  );
}

// Owner only (the api answers 403 to anyone else). The code is shown once: the api keeps only a hash.
function Invite({ groupId }: { groupId: string }) {
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<CreatedJoinCode | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function create() {
    setBusy(true);
    setError(null);
    try {
      setCreated(await unwrap(api.POST("/v1/groups/{id}/join-codes", { params: { path: { id: groupId } } })));
    } catch (e) {
      setError(describeError(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <View style={styles.invite}>
      <Text style={styles.heading}>Invite someone</Text>
      {busy ? <ActivityIndicator /> : <Button title="Create an invite code" onPress={create} />}
      {created !== null && (
        <View style={styles.code}>
          <Text selectable style={styles.codeText}>
            {created.code}
          </Text>
          <Text style={styles.note}>
            Shown once. Up to {created.max_uses} people can use it until {new Date(created.expires_at).toLocaleString()}.
          </Text>
        </View>
      )}
      {error !== null && <Text style={styles.error}>{error}</Text>}
    </View>
  );
}

const styles = StyleSheet.create({
  container: { padding: 16, gap: 8 },
  heading: { fontSize: 18, fontWeight: "600", marginTop: 16 },
  meta: { color: "#666" },
  row: { paddingVertical: 8, borderBottomWidth: StyleSheet.hairlineWidth, borderBottomColor: "#ccc", gap: 2 },
  name: { fontWeight: "600" },
  note: { color: "#666", fontSize: 12 },
  invite: { gap: 8 },
  code: { alignItems: "center", gap: 4, padding: 12, backgroundColor: "#eef", borderRadius: 8 },
  codeText: { fontSize: 24, fontWeight: "700", letterSpacing: 2 },
  error: { color: "#b00020" },
});
