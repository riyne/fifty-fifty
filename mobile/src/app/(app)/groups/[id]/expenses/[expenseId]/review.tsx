import { router, useLocalSearchParams } from "expo-router";
import { useEffect, useRef, useState } from "react";
import { ActivityIndicator, Button, ScrollView, StyleSheet, Text, TextInput, View } from "react-native";

import { unwrap } from "../../../../../../api/client";
import { ApiError, describeError } from "../../../../../../api/errors";
import { canReview } from "../../../../../../api/expenses";
import { api } from "../../../../../../api/instance";
import { createActionKeys } from "../../../../../../api/keys";
import { formatMoney } from "../../../../../../api/money";
import { buildEdit, draftFrom, draftTotal, MAX_ITEMS, type Draft, type EditBody } from "../../../../../../api/review";
import type { Expense } from "../../../../../../api/types";
import { useLoad } from "../../../../../../hooks/useLoad";
import { LoadGate } from "../../../../../../ui/LoadGate";

// The payer's review of a receipt that needs checking: fix the items, tax and tip, then confirm,
// which opens the expense for claims. One button does both calls: the edit (only when something
// changed) and then the confirm with the version the edit returned.

export default function ReviewScreen() {
  const { id, expenseId } = useLocalSearchParams<{ id: string; expenseId: string }>();
  const { state, reload } = useLoad(async () => {
    const [expense, me] = await Promise.all([
      unwrap(api.GET("/v1/groups/{id}/expenses/{expenseId}", { params: { path: { id, expenseId } } })),
      unwrap(api.GET("/v1/me")),
    ]);
    return { expense, me };
  });

  // Load once when the screen opens. Not on every focus (as the other screens do): coming back
  // to this screen must not replace what the payer has typed.
  useEffect(() => {
    void reload();
  }, [reload]);

  // One key per attempt, kept while the same request is retried (see createActionKeys). The
  // version is part of each request, so a changed version is a different request.
  const editKeys = useRef(createActionKeys()).current;
  const confirmKeys = useRef(createActionKeys()).current;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(base: Expense, edit: EditBody | null) {
    setBusy(true);
    setError(null);
    const path = { id, expenseId };
    let version = base.version;
    try {
      if (edit !== null) {
        const body = { ...edit, version };
        const edited = await unwrap(
          api.PATCH("/v1/groups/{id}/expenses/{expenseId}", {
            params: { path, header: { "Idempotency-Key": editKeys.keyFor(JSON.stringify(body)) } },
            body,
          }),
        );
        editKeys.answered();
        version = edited.version;
      }
      await unwrap(
        api.POST("/v1/groups/{id}/expenses/{expenseId}/confirm", {
          params: { path, header: { "Idempotency-Key": confirmKeys.keyFor(String(version)) } },
          body: { version },
        }),
      );
      confirmKeys.answered();
      router.back(); // the expense screen reloads when it comes back and shows it open for claims
    } catch (e) {
      // A stored answer (anything below 500) is replayed for the same key: a new tap needs a new key.
      if (e instanceof ApiError && e.status < 500) {
        editKeys.answered();
        confirmKeys.answered();
      }
      setError(
        e instanceof ApiError && e.code === "version_conflict"
          ? "This expense was changed somewhere else, so nothing was saved. It has been reloaded: check it and try again."
          : describeError(e),
      );
      setBusy(false);
      await reload(); // the form starts again from what the api now holds (a saved edit stays saved)
    }
  }

  return (
    <LoadGate load={state} onRetry={reload}>
      {({ expense, me }) =>
        !canReview(expense, me.id) ? (
          <View style={styles.center}>
            <Text style={styles.centerText}>
              {expense.payer_id !== me.id
                ? "Only the person who paid can review this expense."
                : "This expense is not waiting for review any more."}
            </Text>
            <Button title="Back" onPress={() => router.back()} />
          </View>
        ) : (
          // keyed by version: after a reload that changed the expense the form starts again from it
          <ReviewForm key={expense.version} expense={expense} busy={busy} error={error} onSubmit={submit} />
        )
      }
    </LoadGate>
  );
}

function ReviewForm({
  expense,
  busy,
  error,
  onSubmit,
}: {
  expense: Expense;
  busy: boolean;
  error: string | null;
  onSubmit: (base: Expense, edit: EditBody | null) => void;
}) {
  const [draft, setDraft] = useState<Draft>(() => draftFrom(expense));
  const [problems, setProblems] = useState<string[]>([]);
  const nextKey = useRef(0);
  const total = draftTotal(draft);

  const setItem = (key: string, change: { description?: string; price?: string }) =>
    setDraft((d) => ({ ...d, items: d.items.map((item) => (item.key === key ? { ...item, ...change } : item)) }));

  function confirm() {
    const built = buildEdit(expense, draft);
    if (!built.ok) {
      setProblems(built.problems);
      return;
    }
    if (draft.items.length === 0) {
      setProblems(["Add at least one item."]);
      return;
    }
    setProblems([]);
    onSubmit(expense, built.body);
  }

  return (
    <ScrollView
      contentInsetAdjustmentBehavior="automatic" // start below the translucent iOS header
      keyboardShouldPersistTaps="handled"
      contentContainerStyle={styles.container}
    >
      <Text style={styles.note}>
        Check each line against the receipt. Nobody can claim items until you confirm. The photo itself is not shown here.
      </Text>

      {draft.items.map((item, index) => (
        <View key={item.key} style={styles.row}>
          <TextInput
            style={[styles.input, styles.name]}
            value={item.description}
            onChangeText={(description) => setItem(item.key, { description })}
            placeholder={`Item ${index + 1}`}
            maxLength={200}
            editable={!busy}
          />
          <TextInput
            style={[styles.input, styles.price]}
            value={item.price}
            onChangeText={(price) => setItem(item.key, { price })}
            placeholder="0.00"
            keyboardType="decimal-pad"
            editable={!busy}
          />
          <Button
            title="Remove"
            color="#b00020"
            disabled={busy}
            onPress={() => setDraft((d) => ({ ...d, items: d.items.filter((other) => other.key !== item.key) }))}
          />
        </View>
      ))}
      <Button
        title="Add an item"
        disabled={busy || draft.items.length >= MAX_ITEMS}
        onPress={() =>
          setDraft((d) => ({ ...d, items: [...d.items, { key: `new-${nextKey.current++}`, id: null, description: "", price: "" }] }))
        }
      />

      <View style={styles.row}>
        <Text style={styles.label}>Tax</Text>
        <TextInput
          style={[styles.input, styles.price]}
          value={draft.tax}
          onChangeText={(tax) => setDraft((d) => ({ ...d, tax }))}
          keyboardType="decimal-pad"
          editable={!busy}
        />
      </View>
      <View style={styles.row}>
        <Text style={styles.label}>Tip</Text>
        <TextInput
          style={[styles.input, styles.price]}
          value={draft.tip}
          onChangeText={(tip) => setDraft((d) => ({ ...d, tip }))}
          keyboardType="decimal-pad"
          editable={!busy}
        />
      </View>
      <Text style={styles.total}>Total {total === null ? "—" : formatMoney(total, expense.currency)}</Text>

      {problems.map((problem) => (
        <Text key={problem} style={styles.error}>
          {problem}
        </Text>
      ))}
      {error !== null && <Text style={styles.error}>{error}</Text>}
      {busy ? <ActivityIndicator /> : <Button title="Confirm items" onPress={confirm} />}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  container: { padding: 16, gap: 12 },
  center: { flex: 1, alignItems: "center", justifyContent: "center", gap: 12, padding: 24 },
  centerText: { textAlign: "center" },
  note: { color: "#666" },
  row: { flexDirection: "row", alignItems: "center", gap: 8 },
  label: { flex: 1, fontWeight: "600" },
  input: { borderWidth: 1, borderColor: "#bbb", borderRadius: 8, padding: 10, fontSize: 16, backgroundColor: "#fff" },
  name: { flex: 1 },
  price: { width: 90, textAlign: "right" },
  total: { fontSize: 18, fontWeight: "700", textAlign: "right" },
  error: { color: "#b00020" },
});
