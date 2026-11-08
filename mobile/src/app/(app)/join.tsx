import { router } from "expo-router";
import { useRef, useState } from "react";
import { ActivityIndicator, Button, ScrollView, StyleSheet, Text, TextInput, View } from "react-native";

import { unwrap } from "../../api/client";
import { describeError } from "../../api/errors";
import { api } from "../../api/instance";
import { keyedByValue } from "../../api/keys";

export default function Join() {
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Tapping Join again with the same code after a lost answer is a retry (same key, so the api
  // replays its answer and uses no second code use); a different code gets a new key.
  const keyFor = useRef(keyedByValue()).current;
  const code = text.trim();

  async function join() {
    setBusy(true);
    setError(null);
    try {
      const group = await unwrap(
        api.POST("/v1/join", { params: { header: { "Idempotency-Key": keyFor(code) } }, body: { code } }),
      );
      router.replace({ pathname: "/groups/[id]", params: { id: group.id } });
    } catch (e) {
      setError(
        describeError(e, {
          400: "Enter the 12-character code.",
          404: "That code didn't work. It may be wrong, expired, revoked or used up.",
        }),
      );
      setBusy(false);
    }
  }

  return (
    <ScrollView
      contentInsetAdjustmentBehavior="automatic" // start below the translucent iOS header
      keyboardShouldPersistTaps="handled"
      contentContainerStyle={styles.container}
    >
      <Text style={styles.label}>Code from the group's owner</Text>
      <TextInput
        style={styles.input}
        value={text}
        onChangeText={setText}
        placeholder="ABCD-EFGH-JKMN"
        autoCapitalize="characters"
        autoCorrect={false}
        editable={!busy}
      />
      {busy ? <ActivityIndicator /> : <Button title="Join" onPress={join} disabled={code === ""} />}
      {error !== null && <Text style={styles.error}>{error}</Text>}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  container: { padding: 24, gap: 12 },
  label: { color: "#444" },
  input: { borderWidth: 1, borderColor: "#bbb", borderRadius: 8, padding: 12, fontSize: 17, backgroundColor: "#fff" },
  error: { color: "#b00020" },
});
