import { router, Stack, useLocalSearchParams } from "expo-router";
import { Pressable, RefreshControl, ScrollView, StyleSheet, View } from "react-native";

import { unwrap } from "../../../../api/client";
import { statusShort } from "../../../../api/expenses";
import { api } from "../../../../api/instance";
import { formatMoney } from "../../../../api/money";
import { collectPages } from "../../../../api/pages";
import { balanceParts, transferSentence } from "../../../../api/sentences";
import { settlementTitle, statusWord } from "../../../../api/settlements";
import type { ExpenseSummary, SettlementStatus } from "../../../../api/types";
import { useLoad } from "../../../../hooks/useLoad";
import { useReloadOnReturn } from "../../../../hooks/useReloadOnReturn";
import { Avatar } from "../../../../ui/Avatar";
import { Button } from "../../../../ui/Button";
import { EmptyState } from "../../../../ui/EmptyState";
import { Footer } from "../../../../ui/Footer";
import { Icon } from "../../../../ui/Icon";
import { LoadGate } from "../../../../ui/LoadGate";
import { Pill, type PillTone } from "../../../../ui/Pill";
import { Card, Row, Section } from "../../../../ui/Section";
import { space } from "../../../../ui/theme";
import { Txt } from "../../../../ui/Txt";

const settlementTone: Record<SettlementStatus, PillTone> = {
  pending: "attention",
  unknown: "attention",
  succeeded: "active",
  failed: "bad",
};

/** Green when there is something to claim, rust when this person has to act first, grey otherwise. */
function expenseTone(expense: ExpenseSummary, meId: string): PillTone {
  if (expense.status === "claiming") return "active";
  if (expense.status === "needs_review" && expense.payer_id === meId) return "attention";
  return "quiet";
}

export default function GroupScreen() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const { state, reload } = useLoad(async () => {
    const path = { params: { path: { id } } };
    // one request each; balances and members come from different statements, so a member who
    // joined between the two is simply missing from one list until the next refresh
    const [group, balances, expenses, settlements, me] = await Promise.all([
      unwrap(api.GET("/v1/groups/{id}", path)),
      unwrap(api.GET("/v1/groups/{id}/balances", path)),
      // every page of each list (100 per page), so nothing past the first 100 is lost
      collectPages(async (cursor) => {
        const r = await unwrap(api.GET("/v1/groups/{id}/expenses", { params: { path: { id }, query: { cursor } } }));
        return { items: r.expenses, next: r.next_cursor };
      }),
      collectPages(async (cursor) => {
        const r = await unwrap(api.GET("/v1/groups/{id}/settlements", { params: { path: { id }, query: { cursor } } }));
        return { items: r.settlements, next: r.next_cursor };
      }),
      unwrap(api.GET("/v1/me")),
    ]);
    return { group, balances, expenses, settlements, me };
  });

  useReloadOnReturn(reload);

  return (
    <LoadGate load={state} onRetry={reload}>
      {({ group, balances, expenses, settlements, me }, refreshing) => {
        const currency = balances.currency;
        const names = new Map(balances.balances.map((b) => [b.member_id, b.display_name]));
        const roles = new Map(group.members.map((m) => [m.user_id, m.role]));
        const who = (memberId: string) => ({ name: names.get(memberId) ?? "Someone", isMe: memberId === me.id });
        const mine = balances.balances.find((b) => b.member_id === me.id);
        const toPay = balances.suggested_transfers.filter((t) => t.from_member_id === me.id);
        const others = balances.suggested_transfers.filter((t) => t.from_member_id !== me.id);
        const headline = balanceParts(true, mine?.balance_minor ?? 0, currency);

        return (
          <>
            <Stack.Screen
              options={{
                title: group.name,
                headerRight:
                  group.role !== "owner"
                    ? undefined
                    : () => (
                        <Pressable
                          accessibilityRole="button"
                          accessibilityLabel="Invite people"
                          hitSlop={8}
                          style={styles.headerButton}
                          onPress={() => router.push({ pathname: "/groups/[id]/invite", params: { id: group.id } })}
                        >
                          <Icon name="person.badge.plus" size={22} tone="accent" />
                        </Pressable>
                      ),
              }}
            />
            <ScrollView
              contentInsetAdjustmentBehavior="automatic"
              contentContainerStyle={styles.container}
              refreshControl={<RefreshControl refreshing={refreshing} onRefresh={() => void reload()} />}
            >
              <Card padded>
                <View accessible>
                  <Txt tone="secondary">{headline.label}</Txt>
                  {headline.amount !== null && (
                    <Txt variant="display" tone={headline.tone} numberOfLines={1} adjustsFontSizeToFit>
                      {headline.amount}
                    </Txt>
                  )}
                </View>
                {/* Money already on its way is not in the balance until the provider answers. */}
                {mine !== undefined && mine.reserved_out_minor > 0 && (
                  <Txt variant="small" tone="secondary">
                    {formatMoney(mine.reserved_out_minor, currency)} of it is being paid (simulated).
                  </Txt>
                )}
                {mine !== undefined && mine.reserved_in_minor > 0 && (
                  <Txt variant="small" tone="secondary">
                    {formatMoney(mine.reserved_in_minor, currency)} of it is on its way to you (simulated).
                  </Txt>
                )}
                {toPay.map((t) => (
                  <Button
                    key={t.to_member_id}
                    title={`Pay ${who(t.to_member_id).name} ${formatMoney(t.amount_minor, currency)}`}
                    onPress={() =>
                      router.push({
                        pathname: "/groups/[id]/settle",
                        params: { id: group.id, to: t.to_member_id, amount: String(t.amount_minor) },
                      })
                    }
                  />
                ))}
                {toPay.length > 0 && (
                  <Txt variant="small" tone="secondary">
                    Payments are simulated. No real money moves.
                  </Txt>
                )}
              </Card>

              <Section title="Expenses">
                {expenses.length === 0 ? (
                  <EmptyState icon="doc.text.viewfinder" title="No expenses yet" hint="Add a receipt and its items will show up here to claim." />
                ) : (
                  expenses.map((e) => (
                    <Row
                      key={e.id}
                      title={e.description}
                      subtitle={`Paid by ${who(e.payer_id).isMe ? "you" : who(e.payer_id).name}${e.item_count === 0 ? "" : ` · ${e.item_count} ${e.item_count === 1 ? "item" : "items"}`}`}
                      trailing={
                        <>
                          <Txt variant="strong">{e.status === "parsing" ? "" : formatMoney(e.total_minor, e.currency)}</Txt>
                          <Pill text={statusShort(e.status)} tone={expenseTone(e, me.id)} />
                        </>
                      }
                      onPress={() =>
                        router.push({ pathname: "/groups/[id]/expenses/[expenseId]", params: { id: group.id, expenseId: e.id } })
                      }
                    />
                  ))
                )}
              </Section>

              <Section title="Members">
                {balances.balances.map((b, index) => {
                  const parts = balanceParts(false, b.balance_minor, currency);
                  return (
                    <Row
                      key={b.member_id}
                      title={b.member_id === me.id ? `${b.display_name} (you)` : b.display_name}
                      subtitle={roles.get(b.member_id) === "owner" ? "Owner" : undefined}
                      leading={<Avatar id={b.member_id} index={index} name={b.display_name} />}
                      trailing={
                        <>
                          <Txt variant="small" tone="secondary">
                            {parts.label}
                          </Txt>
                          {parts.amount !== null && (
                            <Txt variant="strong" tone={parts.tone}>
                              {parts.amount}
                            </Txt>
                          )}
                        </>
                      }
                    />
                  );
                })}
              </Section>

              {others.length > 0 && (
                <Section
                  title="Other suggested payments"
                  note="Suggestions only: nothing is recorded until someone pays. They reduce the number of payments but are not always the smallest possible set."
                >
                  {others.map((t) => (
                    <Row
                      key={`${t.from_member_id}-${t.to_member_id}`}
                      title={transferSentence(who(t.from_member_id), who(t.to_member_id), t.amount_minor, currency)}
                      leading={
                        <Avatar
                          id={t.from_member_id}
                          index={balances.balances.findIndex((b) => b.member_id === t.from_member_id)}
                          name={who(t.from_member_id).name}
                        />
                      }
                    />
                  ))}
                </Section>
              )}

              {settlements.length > 0 && (
                <Section title="Payments" note="Payments are simulated: no real money moves.">
                  {settlements.slice(0, 10).map((st) => (
                    <Row
                      key={st.id}
                      title={settlementTitle(st, me.id, (m) => who(m).name, st.currency)}
                      trailing={<Pill text={statusWord(st.status)} tone={settlementTone[st.status]} />}
                      onPress={() =>
                        router.push({
                          pathname: "/groups/[id]/settlements/[settlementId]",
                          params: { id: group.id, settlementId: st.id },
                        })
                      }
                    />
                  ))}
                </Section>
              )}
            </ScrollView>
            <Footer>
              <Button
                grow
                kind={toPay.length > 0 ? "secondary" : "primary"} // one strong button at a time
                title="Add a receipt"
                icon="doc.text.viewfinder"
                onPress={() => router.push({ pathname: "/groups/[id]/receipt", params: { id: group.id } })}
              />
            </Footer>
          </>
        );
      }}
    </LoadGate>
  );
}

const styles = StyleSheet.create({
  container: { padding: space.l, gap: space.xl },
  headerButton: { width: 36, height: 36, alignItems: "center", justifyContent: "center" },
});
