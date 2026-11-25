import * as Haptics from "expo-haptics";
import { router, useLocalSearchParams } from "expo-router";
import { useEffect, useRef, useState } from "react";
import { ScrollView, StyleSheet, View } from "react-native";

import { unwrap } from "../../../../api/client";
import { ApiError, describeError } from "../../../../api/errors";
import { api } from "../../../../api/instance";
import { createActionKeys } from "../../../../api/keys";
import { formatMoney } from "../../../../api/money";
import { minorToInput, parseMinor } from "../../../../api/review";
import { useLoad } from "../../../../hooks/useLoad";
import { Avatar } from "../../../../ui/Avatar";
import { Button } from "../../../../ui/Button";
import { Field } from "../../../../ui/Field";
import { LoadGate } from "../../../../ui/LoadGate";
import { space } from "../../../../ui/theme";
import { Txt } from "../../../../ui/Txt";

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
      void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Success).catch(() => {});
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
            <View style={styles.payee}>
              <Avatar id={to} name={payee} size={64} />
              <Txt variant="title">You pay {payee}</Txt>
            </View>
            <Field
              label={`Amount (${balances.currency})`}
              style={styles.amount}
              value={text}
              onChangeText={setText}
              keyboardType="decimal-pad"
              editable={!busy}
            />
            {Number.isSafeInteger(suggested) && suggested > 0 && (
              <Txt variant="small" tone="secondary" style={styles.centered}>
                Suggested: {formatMoney(suggested, balances.currency)}
              </Txt>
            )}
            {text !== "" && !valid && <Txt tone="danger">Enter an amount above zero, like 12.50.</Txt>}
            {error !== null && <Txt tone="danger">{error}</Txt>}
            <Button
              title={valid ? `Pay ${formatMoney(minor, balances.currency)} (simulated)` : "Pay (simulated)"}
              onPress={() => void pay()}
              busy={busy}
              disabled={!valid}
            />
            <Txt variant="small" tone="secondary" style={styles.centered}>
              Simulated: a mock payment provider answers, and no real money moves.
            </Txt>
          </ScrollView>
        );
      }}
    </LoadGate>
  );
}

const styles = StyleSheet.create({
  container: { padding: space.l, gap: space.l },
  payee: { alignItems: "center", gap: space.m, paddingVertical: space.l },
  amount: { fontSize: 32, fontWeight: "700", textAlign: "center", minHeight: 68 },
  centered: { textAlign: "center" },
});
