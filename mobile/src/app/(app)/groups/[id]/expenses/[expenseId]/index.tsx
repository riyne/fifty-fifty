import * as Haptics from "expo-haptics";
import { router, Stack, useFocusEffect, useLocalSearchParams } from "expo-router";
import { useCallback, useRef, useState } from "react";
import { ActivityIndicator, Alert, RefreshControl, ScrollView, StyleSheet, View } from "react-native";

import { unwrap } from "../../../../../../api/client";
import { ApiError, describeError } from "../../../../../../api/errors";
import {
  canClaim,
  canFinalize,
  canReview,
  canVoid,
  claimSentence,
  finalizeBody,
  statusShort,
  unclaimedCount,
} from "../../../../../../api/expenses";
import { api } from "../../../../../../api/instance";
import { createActionKeys } from "../../../../../../api/keys";
import { formatMinor, formatMoney } from "../../../../../../api/money";
import { useLoad } from "../../../../../../hooks/useLoad";
import { usePolling } from "../../../../../../hooks/usePolling";
import type { Expense } from "../../../../../../api/types";
import { Avatar } from "../../../../../../ui/Avatar";
import { Button } from "../../../../../../ui/Button";
import { ClaimMark } from "../../../../../../ui/ClaimMark";
import { ErrorBar } from "../../../../../../ui/ErrorBar";
import { Footer } from "../../../../../../ui/Footer";
import { LoadGate } from "../../../../../../ui/LoadGate";
import { Pill, type PillTone } from "../../../../../../ui/Pill";
import { Dashes, Receipt, ReceiptLine, TotalLine } from "../../../../../../ui/Receipt";
import { space } from "../../../../../../ui/theme";
import { Txt } from "../../../../../../ui/Txt";

function statusTone(expense: Expense, meId: string): PillTone {
  if (expense.status === "claiming") return "active";
  return canReview(expense, meId) ? "attention" : "quiet";
}

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
      void reload({ spinner: false });
    }, [reload]),
  );

  // A receipt that was just uploaded is `parsing` until a worker has read it: look again every 2 s
  // (without the refresh spinner) for up to a minute, then leave it to pull to refresh.
  const parsing = state.status === "ready" && state.data.expense.status === "parsing";
  const { gaveUp, restart } = usePolling(parsing, () => void reload({ silent: true }), 2000, 60_000);

  // One key per tap on Finalize or Void, kept while the same request is retried (see createActionKeys).
  const finalizeKeys = useRef(createActionKeys()).current;
  const voidKeys = useRef(createActionKeys()).current;
  const [busy, setBusy] = useState<"finalize" | "void" | null>(null);
  const [error, setError] = useState<string | null>(null);

  /** Runs finalize or void, shows its failure, and reloads whatever the outcome (the screen may be stale). */
  async function perform(what: "finalize" | "void", call: () => Promise<unknown>, answered: () => void) {
    setBusy(what);
    setError(null);
    try {
      await call();
      answered();
      void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success).catch(() => {});
    } catch (e) {
      // A stored answer (anything below 500) is replayed for the same key: a new tap needs a new key.
      // No answer at all, or a 5xx (never stored), is retried with the same key.
      if (e instanceof ApiError && e.status < 500) answered();
      setError(describeError(e));
    } finally {
      await reload({ spinner: false });
      setBusy(null);
    }
  }

  // Claims are drawn as soon as they are tapped, and several can be on their way at once. `drawn`
  // (item id -> mine or not) covers the loaded answer until every one of them has been answered and
  // the screen reloaded: the newest reload then started after all of them, so it has them all.
  const [drawn, setDrawn] = useState<ReadonlyMap<string, boolean>>(new Map());
  const inFlight = useRef(new Set<string>()).current;

  async function toggleClaim(itemId: string, mine: boolean) {
    if (inFlight.has(itemId)) return;
    if (inFlight.size === 0) setError(null);
    inFlight.add(itemId);
    setDrawn((previous) => new Map(previous).set(itemId, !mine));
    void Haptics.selectionAsync().catch(() => {});
    const path = { params: { path: { id, expenseId, itemId } } };
    try {
      if (mine) {
        await unwrap(api.DELETE("/v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", path));
      } else {
        await unwrap(api.PUT("/v1/groups/{id}/expenses/{expenseId}/items/{itemId}/claim", path));
      }
    } catch (e) {
      setError(e instanceof ApiError && e.status === 409 ? "This expense is no longer open for claims." : describeError(e));
    } finally {
      await reload({ spinner: false });
      inFlight.delete(itemId);
      if (inFlight.size === 0) setDrawn(new Map());
    }
  }

  return (
    <LoadGate load={state} onRetry={reload}>
      {({ group, balances, expense, me }, refreshing) => {
        const names = new Map(balances.balances.map((b) => [b.member_id, b.display_name]));
        const nameOf = (memberId: string) => names.get(memberId) ?? "Someone";
        const payerIsMe = expense.payer_id === me.id;
        const memberIds = balances.balances.map((b) => b.member_id);
        const open = unclaimedCount(expense);
        const claiming = canClaim(expense);

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

        /** Who the item is drawn as claimed by: the api's answer, with this person's own taps on top. */
        const claimersOf = (item: Expense["items"][number]) => {
          const mine = drawn.get(item.id);
          if (mine === undefined || mine === item.claimed_by.includes(me.id)) return item.claimed_by;
          return mine ? [...item.claimed_by, me.id] : item.claimed_by.filter((memberId) => memberId !== me.id);
        };
        const drawnOpen = expense.items.filter((item) => claimersOf(item).length === 0).length;

        return (
          <>
            <Stack.Screen options={{ title: expense.description }} />
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
              <Receipt>
                <View style={styles.head}>
                  {expense.status === "parsing" ? (
                    <Txt variant="title" style={styles.centered}>
                      Reading the receipt
                    </Txt>
                  ) : (
                    <Txt variant="display" numberOfLines={1} adjustsFontSizeToFit style={expense.status === "void" && styles.voided}>
                      {formatMoney(expense.total_minor, expense.currency)}
                    </Txt>
                  )}
                  <Txt tone="secondary" style={styles.centered}>
                    Paid by {payerIsMe ? "you" : nameOf(expense.payer_id)}
                  </Txt>
                  <Pill text={statusShort(expense.status)} tone={statusTone(expense, me.id)} />
                </View>

                {expense.status === "parsing" && (
                  <View style={styles.message}>
                    {!gaveUp && <ActivityIndicator />}
                    <Txt tone="secondary" style={styles.centered}>
                      {gaveUp
                        ? "Still reading the receipt. Pull down to check again. If this keeps happening, the worker may not be running."
                        : "This usually takes a few seconds."}
                    </Txt>
                  </View>
                )}
                {expense.status === "needs_review" && (
                  <View style={styles.message}>
                    <Txt tone="secondary" style={styles.centered}>
                      {payerIsMe ? "You need" : `${nameOf(expense.payer_id)} needs`} to check the items against the receipt
                      before anyone can claim them.
                    </Txt>
                  </View>
                )}

                {expense.items.length > 0 && (
                  <>
                    <Dashes />
                    {claiming && (
                      <Txt variant="small" tone="secondary" style={styles.hint}>
                        {drawnOpen === 0
                          ? "Tap what you had. Everything is claimed."
                          : `Tap what you had. ${drawnOpen} ${drawnOpen === 1 ? "item is" : "items are"} still unclaimed.`}
                      </Txt>
                    )}
                    {expense.items.map((item) => {
                      const claimers = claimersOf(item);
                      const mine = claimers.includes(me.id);
                      const sentence = claimSentence({ claimed_by: claimers }, me.id, nameOf);
                      const showClaims = claiming || claimers.length > 0;
                      return (
                        <ReceiptLine
                          key={item.id}
                          mark={showClaims ? <ClaimMark share={mine ? 1 / claimers.length : 0} /> : undefined}
                          title={item.description}
                          detail={
                            showClaims ? (
                              <View style={styles.claimers}>
                                {claimers.map((memberId) => (
                                  <Avatar key={memberId} id={memberId} name={nameOf(memberId)} size={18} />
                                ))}
                                <Txt variant="small" tone="secondary" style={styles.sentence}>
                                  {sentence}
                                </Txt>
                              </View>
                            ) : undefined
                          }
                          price={formatMinor(item.price_minor)}
                          checked={mine}
                          accessibilityLabel={`${item.description}, ${formatMoney(item.price_minor, expense.currency)}${showClaims ? `, ${sentence}` : ""}`}
                          onPress={claiming && busy === null ? () => void toggleClaim(item.id, mine) : undefined}
                        />
                      );
                    })}
                    <Dashes />
                    <TotalLine label="Subtotal" amount={formatMinor(expense.subtotal_minor)} />
                    <TotalLine label="Tax" amount={formatMinor(expense.tax_minor)} />
                    <TotalLine label="Tip" amount={formatMinor(expense.tip_minor)} />
                    <TotalLine strong label="TOTAL" amount={formatMoney(expense.total_minor, expense.currency)} />
                  </>
                )}
              </Receipt>

              {claiming && (
                <Txt variant="small" tone="secondary" style={styles.note}>
                  Items with more than one person are split evenly. Tax and tip are shared in proportion to what each person
                  claimed. {payerIsMe ? "Finalize" : `${nameOf(expense.payer_id)} finalizes`} when everyone has claimed.
                </Txt>
              )}
              {canVoid(expense, me.id, group.role) && (
                <Button kind="danger" title="Void this expense" onPress={confirmVoid} busy={busy === "void"} disabled={busy !== null} />
              )}
            </ScrollView>

            {/* outside the scroll, so a claim that failed is seen wherever the list is scrolled to */}
            {error !== null && <ErrorBar text={error} />}
            {canReview(expense, me.id) && (
              <Footer>
                <Button
                  grow
                  title="Review the items"
                  onPress={() => router.push({ pathname: "/groups/[id]/expenses/[expenseId]/review", params: { id, expenseId } })}
                />
              </Footer>
            )}
            {canFinalize(expense, me.id) && (
              <Footer>
                <Button
                  grow
                  title={drawnOpen === 0 ? "Finalize" : `Finalize (${drawnOpen} unclaimed)`}
                  onPress={confirmFinalize}
                  busy={busy === "finalize"}
                  disabled={busy !== null || drawn.size > 0} // the request is built from loaded claims, so wait for them
                />
              </Footer>
            )}
          </>
        );
      }}
    </LoadGate>
  );
}

const styles = StyleSheet.create({
  container: { padding: space.l, gap: space.l },
  head: { alignItems: "center", gap: space.xs, paddingHorizontal: space.l, paddingVertical: space.s },
  centered: { textAlign: "center" },
  voided: { textDecorationLine: "line-through" },
  message: { gap: space.m, padding: space.l, paddingBottom: space.s },
  hint: { textAlign: "center", paddingHorizontal: space.l, paddingBottom: space.s },
  claimers: { flexDirection: "row", alignItems: "center", flexWrap: "wrap", gap: space.xs },
  sentence: { flexShrink: 1 },
  note: { paddingHorizontal: space.l },
});
