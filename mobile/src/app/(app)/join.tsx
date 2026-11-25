import { router } from "expo-router";
import { useRef, useState } from "react";
import { ScrollView, StyleSheet } from "react-native";

import { unwrap } from "../../api/client";
import { describeError } from "../../api/errors";
import { api } from "../../api/instance";
import { keyedByValue } from "../../api/keys";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { space } from "../../ui/theme";
import { Txt } from "../../ui/Txt";

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
      <Txt tone="secondary">Ask the group's owner for their invite code.</Txt>
      <Field
        label="Invite code"
        style={styles.code}
        value={text}
        onChangeText={setText}
        placeholder="ABCD-EFGH-JKMN"
        autoCapitalize="characters"
        autoCorrect={false}
        autoFocus
        editable={!busy}
      />
      {error !== null && <Txt tone="danger">{error}</Txt>}
      <Button title="Join" onPress={() => void join()} busy={busy} disabled={code === ""} />
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  container: { padding: space.l, gap: space.l },
  code: { fontFamily: "ui-monospace", fontSize: 20, textAlign: "center", minHeight: 56 },
});
