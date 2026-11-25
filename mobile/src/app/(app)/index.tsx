import { router, Stack } from "expo-router";
import { FlatList, Pressable, StyleSheet, View } from "react-native";

import { unwrap } from "../../api/client";
import { api } from "../../api/instance";
import { collectPages } from "../../api/pages";
import { balanceParts } from "../../api/sentences";
import { useGroupBalances } from "../../hooks/useGroupBalances";
import { useLoad } from "../../hooks/useLoad";
import { useReloadOnReturn } from "../../hooks/useReloadOnReturn";
import { Avatar } from "../../ui/Avatar";
import { Button } from "../../ui/Button";
import { EmptyState } from "../../ui/EmptyState";
import { Footer } from "../../ui/Footer";
import { Icon } from "../../ui/Icon";
import { LoadGate } from "../../ui/LoadGate";
import { Card, Row } from "../../ui/Section";
import { space } from "../../ui/theme";
import { Txt } from "../../ui/Txt";

export default function Groups() {
  const { state, reload } = useLoad(async () => {
    const [groups, me] = await Promise.all([
      // every page: a user in more than 100 groups would otherwise lose the rest
      collectPages(async (cursor) => {
        const r = await unwrap(api.GET("/v1/groups", { params: { query: { cursor } } }));
        return { items: r.groups, next: r.next_cursor };
      }),
      unwrap(api.GET("/v1/me")),
    ]);
    return { groups, me };
  });
  const ready = state.status === "ready" ? state.data : null;
  const balances = useGroupBalances(ready?.groups ?? null, ready?.me.id ?? null);

  useReloadOnReturn(reload);

  return (
    <LoadGate load={state} onRetry={reload}>
      {({ groups, me }, refreshing) => (
        <>
          <Stack.Screen
            options={{
              headerRight: () => (
                <Pressable accessibilityRole="button" accessibilityLabel="Settings and sign out" hitSlop={8} style={styles.account} onPress={() => router.push({ pathname: "/settings", params: { name: me.display_name } })}>
                  <Icon name="person.crop.circle" size={24} tone="accent" />
                </Pressable>
              ),
            }}
          />
          <FlatList
            data={groups}
            contentInsetAdjustmentBehavior="automatic" // start below the translucent iOS header
            contentContainerStyle={styles.list}
            keyExtractor={(group) => group.id}
            refreshing={refreshing}
            onRefresh={() => void reload()}
            ListEmptyComponent={
              <EmptyState icon="person.2" title="No groups yet" hint="Create one for a trip or a dinner, or join a friend's group with their code." />
            }
            renderItem={({ item }) => {
              const minor = balances.get(item.id);
              const parts = minor === undefined ? null : balanceParts(true, minor, item.currency);
              return (
                <Card>
                  <Row
                    title={item.name}
                    subtitle={`${item.currency} · ${item.role}`}
                    leading={<Avatar id={item.id} name={item.name} size={42} />}
                    trailing={
                      parts === null ? undefined : (
                        <>
                          <Txt variant="small" tone="secondary">
                            {parts.amount === null ? "Settled up" : parts.label}
                          </Txt>
                          {parts.amount !== null && (
                            <Txt variant="strong" tone={parts.tone}>
                              {parts.amount}
                            </Txt>
                          )}
                        </>
                      )
                    }
                    onPress={() => router.push({ pathname: "/groups/[id]", params: { id: item.id } })}
                  />
                </Card>
              );
            }}
            ItemSeparatorComponent={() => <View style={styles.gap} />}
          />
          <Footer>
            <Button grow kind="secondary" title="Join with code" onPress={() => router.push("/join")} />
            <Button grow title="New group" icon="plus" onPress={() => router.push("/groups/new")} />
          </Footer>
        </>
      )}
    </LoadGate>
  );
}

const styles = StyleSheet.create({
  account: { width: 36, height: 36, alignItems: "center", justifyContent: "center" },
  list: { padding: space.l },
  gap: { height: space.m },
});
