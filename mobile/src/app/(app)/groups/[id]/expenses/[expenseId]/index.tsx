import { router, Stack, useFocusEffect, useLocalSearchParams } from "expo-router";
import { useCallback, useRef, useState } from "react";
import { ActivityIndicator, Alert, Button, RefreshControl, ScrollView, StyleSheet, Text, View } from "react-native";

import { unwrap } from "../../../../../../api/client";
import { ApiError, describeError } from "../../../../../../api/errors";
import {
  canClaim,
  canFinalize,
  canReview,
  canVoid,
  claimSentence,
  finalizeBody,
  statusLabel,
  unclaimedCount,
} from "../../../../../../api/expenses";
import { api } from "../../../../../../api/instance";
import { createActionKeys } from "../../../../../../api/keys";
import { formatMoney } from "../../../../../../api/money";
import { useLoad } from "../../../../../../hooks/useLoad";
import { usePolling } from "../../../../../../hooks/usePolling";
import { LoadGate } from "../../../../../../ui/LoadGate";

export default function ExpenseScreen() {
  const { id, expenseId } = useLocalSearchParams<{ id: string; expenseId: string }>();
  const { state, reload } = useLoad(async () => {
    const group = { params: { path: { id } } };
    const expense = { params: { path: { id, expenseId } } };
    const [groupDetail, balances, detail, me] = await Promise.all([
      unwrap(api.GET("/v1/groups/{id}", group)),
      unwrap(api.GET("/v1/groups/{id}/balances", group)), // the members' display names
      unwrap(api.GET("/v1/groups/{id}/expenses/{expenseId}", expense)),
      unwrap(api.GET("/v1/me")),
    ]);
    return { group: groupDetail, balances, expense: detail, me };
  });

  useFocusEffect(
    useCallback(() => {
      void reload();
    }, [reload]),
  );

  // A receipt that was just uploaded is `parsing` until a worker has read it: look again every 2 s
  // (without the refresh spinner) for up to a minute, then leave it to pull to refresh.
  const parsing = state.status === "ready" && state.data.expense.status === "parsing";
  const { gaveUp, restart } = usePolling(parsing, () => void reload({ silent: true }), 2000, 60_000);

  // One key per tap on Finalize or Void, kept while the same request is retried (see createActionKeys).
  const finalizeKeys = useRef(createActionKeys()).current;
  const voidKeys = useRef(createActionKeys()).current;
  const [busy, setBusy] = useState<string | null>(null); // an item id, "finalize" or "void"
  const [error, setError] = useState<string | null>(null);

  /** Runs one action, shows its failure, and reloads whatever the outcome (the screen may be stale). */
  async function perform(what: string, call: () => Promise<unknown>, answered?: () => void) {
    setBusy(what);
    setError(null);
    try {
      await call();
      answered?.();
    } catch (e) {
      // A stored answer (anything below 500) is replayed for the same key: a new tap needs a new key.
      // No answer at all, or a 5xx (never stored), is retried with the same key.
      if (e instanceof ApiError && e.status < 500) answered?.();
      setError(describeError(e));
    } finally {
      setBusy(null);
      await reload();
    }
  }

  return (
    <LoadGate load={state} onRetry={reload}>
      {({ group, balances, expense, me }, refreshing) => {
        const names = new Map(balances.balances.map((b) => [b.member_id, b.display_name]));
        const nameOf = (memberId: string) => names.get(memberId) ?? "Someone";
        const payerIsMe = expense.payer_id === me.id;
        const path = (itemId: string) => ({ params: { path: { id, expenseId, itemId } } });
        const memberIds = balances.balances.map((b) => b.member_id);
        const open = unclaimedCount(expense);

        function confirmFinalize() {
          const text =
            open === 0
              ? "Everyone's shares are fixed and added to the group's balances."
              : `${open} ${open === 1 ? "item has" : "items have"} nobody on ${open === 1 ? "it" : "them"}. They will be split evenly among all ${memberIds.length} members. Everyone's shares are fixed and added to the group's balances.`;
          Alert.alert("Finalize this expense?", text, [
            { text: "Cancel", style: "cancel" },
            {
              text: open === 0 ? "Finalize" : "Split the rest and finalize",
              onPress: () => {
                const body = finalizeBody(expense, memberIds);
                void perform(
                  "finalize",
                  () =>
                    unwrap(
                      api.POST("/v1/groups/{id}/expenses/{expenseId}/finalize", {
                        params: {
                          path: { id, expenseId },
                          header: { "Idempotency-Key": finalizeKeys.keyFor(JSON.stringify(body)) },
                        },
                        body,
                      }),
                    ),
                  finalizeKeys.answered,
                );
              },
            },
          ]);
        }

        function confirmVoid() {
          Alert.alert(
            "Void this expense?",
            "Everyone's balances go back to what they would have been without it. The record of both stays in the ledger.",
            [
              { text: "Cancel", style: "cancel" },
              {
                text: "Void",
                style: "destructive",
                onPress: () =>
                  void perform(
                    "void",
                    () =>
                      unwrap(
                        api.POST("/v1/groups/{id}/expenses/{expenseId}/void", {
                          params: { path: { id, expenseId }, header: { "Idempotency-Key": voidKeys.keyFor("") } },
                        }),
                      ),
                    voidKeys.answered,
                  ),
              },
            ],
          );
        }

        function toggleClaim(itemId: string, mine: boolean) {
          void perform(itemId, async () => {
            try {
              if (mine) {
                await unwrap(api.DELETE("/v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", path(itemId)));
              } else {
                await unwrap(api.PUT("/v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", path(itemId)));
              }
            } catch (e) {
              if (e instanceof ApiError && e.status === 409) {
                throw new ApiError(409, e.code, "This expense is no longer open for claims.");
              }
              throw e;
            }
          });
        }

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
            <Stack.Screen options={{ title: expense.description }} />
            <Text style={styles.title}>{formatMoney(expense.total_minor, expense.currency)}</Text>
            <Text style={styles.meta}>
              {statusLabel(expense.status)} · paid by {payerIsMe ? "you" : nameOf(expense.payer_id)}
            </Text>

            {expense.status === "parsing" && (
              <View style={styles.banner}>
                {!gaveUp && <ActivityIndicator />}
                <Text style={styles.bannerText}>
                  {gaveUp
                    ? "Still reading the receipt. Pull down to check again. If this keeps happening, the worker may not be running."
                    : "Reading the receipt. This usually takes a few seconds."}
                </Text>
              </View>
            )}
            {expense.status === "needs_review" && (
              <View style={styles.banner}>
                <Text style={styles.bannerText}>
                  {payerIsMe ? "You" : nameOf(expense.payer_id)} {payerIsMe ? "need" : "needs"} to check the items against
                  the receipt before anyone can claim them.
                </Text>
              </View>
            )}

            <Text style={styles.heading}>Items</Text>
            {expense.items.map((item) => {
              const mine = item.claimed_by.includes(me.id);
              return (
                <View key={item.id} style={styles.row}>
                  <View style={styles.itemText}>
                    <Text style={styles.name}>
                      {item.description} · {formatMoney(item.price_minor, expense.currency)}
                    </Text>
                    <Text style={styles.meta}>{claimSentence(item, me.id, nameOf)}</Text>
                  </View>
                  {canClaim(expense) &&
                    (busy === item.id ? (
                      <ActivityIndicator />
                    ) : (
                      <Button
                        title={mine ? "Not mine" : "Mine"}
                        onPress={() => toggleClaim(item.id, mine)}
                        disabled={busy !== null}
                      />
                    ))}
                </View>
              );
            })}

            <View style={styles.totals}>
              <Text>Subtotal {formatMoney(expense.subtotal_minor, expense.currency)}</Text>
              <Text>Tax {formatMoney(expense.tax_minor, expense.currency)}</Text>
              <Text>Tip {formatMoney(expense.tip_minor, expense.currency)}</Text>
              <Text style={styles.name}>Total {formatMoney(expense.total_minor, expense.currency)}</Text>
            </View>
            {expense.status === "claiming" && (
              <Text style={styles.note}>
                Tax and tip are shared in proportion to what each person claimed. The payer finalizes when everyone has
                claimed their items.
              </Text>
            )}

            {error !== null && <Text style={styles.error}>{error}</Text>}

            {canReview(expense, me.id) && (
              <View style={styles.actions}>
                <Button
                  title="Review the items"
                  onPress={() => router.push({ pathname: "/groups/[id]/expenses/[expenseId]/review", params: { id, expenseId } })}
                />
              </View>
            )}
            {canFinalize(expense, me.id) && (
              <View style={styles.actions}>
                {busy === "finalize" ? (
                  <ActivityIndicator />
                ) : (
                  <Button
                    title={open === 0 ? "Finalize" : `Finalize (${open} unclaimed)`}
                    onPress={confirmFinalize}
                    disabled={busy !== null}
                  />
                )}
              </View>
            )}
            {canVoid(expense, me.id, group.role) && (
              <View style={styles.actions}>
                {busy === "void" ? (
                  <ActivityIndicator />
                ) : (
                  <Button title="Void this expense" color="#b00020" onPress={confirmVoid} disabled={busy !== null} />
                )}
              </View>
            )}
          </ScrollView>
        );
      }}
    </LoadGate>
  );
}

const styles = StyleSheet.create({
  container: { padding: 16, gap: 8 },
  title: { fontSize: 28, fontWeight: "700" },
  heading: { fontSize: 18, fontWeight: "600", marginTop: 16 },
  meta: { color: "#666" },
  row: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "space-between",
    gap: 12,
    paddingVertical: 8,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: "#ccc",
  },
  itemText: { flex: 1, gap: 2 },
  name: { fontWeight: "600" },
  totals: { marginTop: 8, gap: 2 },
  note: { color: "#666", fontSize: 12 },
  error: { color: "#b00020" },
  actions: { marginTop: 16 },
  banner: { flexDirection: "row", alignItems: "center", gap: 12, padding: 12, borderRadius: 8, backgroundColor: "#eef" },
  bannerText: { flex: 1 },
});
