import { useState } from "react";
import { StyleSheet, View } from "react-native";
import { SafeAreaView } from "react-native-safe-area-context";

import { session } from "../auth";
import { signInWithOidc } from "../auth/oidc";
import { Button } from "../ui/Button";
import { ClaimMark } from "../ui/ClaimMark";
import { space } from "../ui/theme";
import { Txt } from "../ui/Txt";

export default function Login() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onSignIn() {
    setBusy(true);
    setError(null);
    try {
      const tokens = await signInWithOidc();
      if (tokens !== null) await session.signIn(tokens.idToken, tokens.refreshToken); // the layout then leaves this screen
    } catch (e) {
      setError(e instanceof Error ? e.message : "login failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <SafeAreaView style={styles.screen}>
      <View style={styles.intro}>
        <ClaimMark share={0.5} size={84} />
        <Txt variant="display" accessibilityRole="header">
          Fifty Fifty
        </Txt>
        <Txt tone="secondary" style={styles.centered}>
          Photograph the receipt, tap what you had, and everyone pays their share.
        </Txt>
      </View>
      <View style={styles.actions}>
        {error !== null && (
          <Txt tone="danger" style={styles.centered}>
            {error}
          </Txt>
        )}
        <Button title="Sign in" onPress={() => void onSignIn()} busy={busy} />
        <Txt variant="small" tone="secondary" style={styles.centered}>
          Settlement is simulated. No real money moves.
        </Txt>
        {__DEV__ && (
          <Txt variant="small" tone="secondary" style={styles.centered}>
            Development accounts: alice@example.com, bob@example.com, carol@example.com (password: password)
          </Txt>
        )}
      </View>
    </SafeAreaView>
  );
}

const styles = StyleSheet.create({
  screen: { flex: 1, paddingHorizontal: space.xl },
  intro: { flex: 1, alignItems: "center", justifyContent: "center", gap: space.l },
  actions: { gap: space.m, paddingBottom: space.l },
  centered: { textAlign: "center" },
});
