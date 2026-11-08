import { router } from "expo-router";
import { useState } from "react";
import { ActivityIndicator, Button, ScrollView, StyleSheet, Text, TextInput, View } from "react-native";

import { unwrap } from "../../../api/client";
import { describeError } from "../../../api/errors";
import { api } from "../../../api/instance";
import type { Currency } from "../../../api/types";

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
      <Text style={styles.label}>Name</Text>
      <TextInput
        style={styles.input}
        value={name}
        onChangeText={setName}
        placeholder="Lisbon trip"
        maxLength={100}
        editable={!busy}
      />
      <Text style={styles.label}>Currency (one per group, cannot be changed)</Text>
      <View style={styles.choices}>
        {(["CAD", "USD"] as const).map((code) => (
          <Button
            key={code}
            title={currency === code ? `✓ ${code}` : code}
            onPress={() => setCurrency(code)}
            disabled={busy}
          />
        ))}
      </View>
      {busy ? <ActivityIndicator /> : <Button title="Create group" onPress={create} disabled={trimmed === ""} />}
      {error !== null && <Text style={styles.error}>{error}</Text>}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  container: { padding: 24, gap: 12 },
  label: { color: "#444" },
  input: { borderWidth: 1, borderColor: "#bbb", borderRadius: 8, padding: 12, fontSize: 17, backgroundColor: "#fff" },
  choices: { flexDirection: "row", gap: 24 },
  error: { color: "#b00020" },
});
