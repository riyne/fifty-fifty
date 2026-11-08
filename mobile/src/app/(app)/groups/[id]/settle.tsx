import { router, useLocalSearchParams } from "expo-router";
import { useEffect, useRef, useState } from "react";
import { ActivityIndicator, Button, ScrollView, StyleSheet, Text, TextInput, View } from "react-native";

import { unwrap } from "../../../../api/client";
import { ApiError, describeError } from "../../../../api/errors";
import { api } from "../../../../api/instance";
import { createActionKeys } from "../../../../api/keys";
import { formatMoney } from "../../../../api/money";
import { minorToInput, parseMinor } from "../../../../api/review";
import { useLoad } from "../../../../hooks/useLoad";
import { LoadGate } from "../../../../ui/LoadGate";

// Pay another member through the SIMULATED payment provider. Arrives from a suggested transfer
// with the payee and the suggested amount; the amount can be changed, and the api refuses more
// than can be settled between the two right now (409 `exceeds_available`).

export default function Settle() {
  const { id, to, amount } = useLocalSearchParams<{ id: string; to: string; amount: string }>();
  const suggested = Number(amount);
  const { state, reload } = useLoad(() => unwrap(api.GET("/v1/groups/{id}/balances", { params: { path: { id } } })));
  // Load once when the screen opens, not on every focus: a reload must not replace what was typed.
  useEffect(() => {
    void reload();
  }, [reload]);

  const [text, setText] = useState(() => (Number.isSafeInteger(suggested) && suggested > 0 ? minorToInput(suggested) : ""));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // One key per tap, kept while the same request is retried (see createActionKeys).
  const keys = useRef(createActionKeys()).current;
  const minor = parseMinor(text);
  const valid = minor !== null && minor > 0;

  async function pay() {
    if (!valid) return;
    setBusy(true);
    setError(null);
    const body = { to_member_id: to, amount_minor: minor };
    try {
      const settlement = await unwrap(
        api.POST("/v1/groups/{id}/settlements", {
          params: { path: { id }, header: { "Idempotency-Key": keys.keyFor(JSON.stringify(body)) } },
          body,
        }),
      );
      keys.answered();
      router.replace({ pathname: "/groups/[id]/settlements/[settlementId]", params: { id, settlementId: settlement.id } });
    } catch (e) {
      // A stored answer (anything below 500, a 409 included) is replayed for the same key.
      if (e instanceof ApiError && e.status < 500) keys.answered();
      setError(describeError(e));
      setBusy(false);
    }
  }

  return (
    <LoadGate load={state} onRetry={reload}>
      {(balances) => {
        const payee = balances.balances.find((b) => b.member_id === to)?.display_name ?? "this member";
        return (
          <ScrollView
            contentInsetAdjustmentBehavior="automatic" // start below the translucent iOS header
            keyboardShouldPersistTaps="handled"
            contentContainerStyle={styles.container}
          >
            <Text style={styles.title}>You pay {payee}</Text>
            <View style={styles.banner}>
              <Text>Simulated: a mock payment provider answers, and no real money moves.</Text>
            </View>

            <Text style={styles.label}>Amount ({balances.currency})</Text>
            <TextInput
              style={styles.input}
              value={text}
              onChangeText={setText}
              keyboardType="decimal-pad"
              editable={!busy}
            />
            {Number.isSafeInteger(suggested) && suggested > 0 && (
              <Text style={styles.note}>Suggested: {formatMoney(suggested, balances.currency)}</Text>
            )}
            {text !== "" && !valid && <Text style={styles.error}>Enter an amount above zero, like 12.50.</Text>}
            {error !== null && <Text style={styles.error}>{error}</Text>}
            {busy ? <ActivityIndicator /> : <Button title="Pay (simulated)" onPress={pay} disabled={!valid} />}
          </ScrollView>
        );
      }}
    </LoadGate>
  );
}

const styles = StyleSheet.create({
  container: { padding: 24, gap: 12 },
  title: { fontSize: 22, fontWeight: "700" },
  banner: { padding: 12, borderRadius: 8, backgroundColor: "#eef" },
  label: { color: "#444" },
  input: { borderWidth: 1, borderColor: "#bbb", borderRadius: 8, padding: 12, fontSize: 17, backgroundColor: "#fff" },
  note: { color: "#666", fontSize: 12 },
  error: { color: "#b00020" },
});
