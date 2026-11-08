import { useState } from "react";
import { ActivityIndicator, Button, StyleSheet, Text, View } from "react-native";

import { session } from "../auth";
import { signInWithOidc } from "../auth/oidc";

export default function Login() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onSignIn() {
    setBusy(true);
    setError(null);
    try {
      const idToken = await signInWithOidc();
      if (idToken !== null) await session.signIn(idToken); // the layout then leaves this screen
    } catch (e) {
      setError(e instanceof Error ? e.message : "login failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <View style={styles.container}>
      <Text style={styles.title}>Fifty Fifty</Text>
      <Text style={styles.note}>Split a receipt with friends. Settlement is simulated.</Text>
      {busy ? <ActivityIndicator /> : <Button title="Sign in" onPress={onSignIn} />}
      {error !== null && <Text style={styles.error}>{error}</Text>}
      {__DEV__ && (
        <Text style={styles.small}>
          Development accounts: alice@example.com, bob@example.com, carol@example.com (password: password)
        </Text>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, alignItems: "center", justifyContent: "center", gap: 16, padding: 24 },
  title: { fontSize: 28, fontWeight: "600" },
  note: { textAlign: "center", color: "#444" },
  error: { color: "#b00020", textAlign: "center" },
  small: { fontSize: 12, color: "#666", textAlign: "center" },
});
