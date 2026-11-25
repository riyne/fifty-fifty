import { router } from "expo-router";
import { useState } from "react";
import { ScrollView, StyleSheet } from "react-native";

import { unwrap } from "../../../api/client";
import { describeError } from "../../../api/errors";
import { api } from "../../../api/instance";
import type { Currency } from "../../../api/types";
import { Button } from "../../../ui/Button";
import { Choice } from "../../../ui/Choice";
import { Field } from "../../../ui/Field";
import { space } from "../../../ui/theme";
import { Txt } from "../../../ui/Txt";

export default function NewGroup() {
  const [name, setName] = useState("");
  const [currency, setCurrency] = useState<Currency>("CAD");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const trimmed = name.trim();

  async function create() {
    setBusy(true);
    setError(null);
    try {
      const group = await unwrap(api.POST("/v1/groups", { body: { name: trimmed, currency } }));
      router.replace({ pathname: "/groups/[id]", params: { id: group.id } });
    } catch (e) {
      setError(describeError(e));
      setBusy(false);
    }
  }

  return (
    <ScrollView
      contentInsetAdjustmentBehavior="automatic" // start below the translucent iOS header
      keyboardShouldPersistTaps="handled"
      contentContainerStyle={styles.container}
    >
      <Field
        label="Name"
        value={name}
        onChangeText={setName}
        placeholder="Lisbon trip"
        maxLength={100}
        autoFocus
        returnKeyType="done"
        editable={!busy}
      />
      <Choice label="Currency (cannot be changed later)" options={["CAD", "USD"]} value={currency} onChange={setCurrency} disabled={busy} />
      {error !== null && <Txt tone="danger">{error}</Txt>}
      <Button title="Create group" onPress={() => void create()} busy={busy} disabled={trimmed === ""} />
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  container: { padding: space.l, gap: space.l },
});
