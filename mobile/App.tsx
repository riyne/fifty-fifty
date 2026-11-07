import { StatusBar } from "expo-status-bar";
import { useEffect, useState } from "react";
import { StyleSheet, Text, View } from "react-native";

import { createApi, unwrap } from "./src/api/client";
import { API_URL } from "./src/config";
import { newIdempotencyKey } from "./src/api/keys";
import { formatMoney } from "./src/api/money";

// Step 8.1 placeholder: shows that the toolchain, the typed client and the api connect.
// Replaced by the login screen in 8.2.
const api = createApi({ baseUrl: API_URL, getToken: () => null });

export default function App() {
  const [status, setStatus] = useState("checking the api...");

  useEffect(() => {
    unwrap(api.GET("/healthz"))
      .then((body) => setStatus(`api at ${API_URL}: ${body.status}`))
      .catch((error: unknown) => setStatus(`api at ${API_URL}: ${error instanceof Error ? error.message : "failed"}`));
  }, []);

  return (
    <View style={styles.container}>
      <Text style={styles.title}>Fifty Fifty</Text>
      <Text>{status}</Text>
      <Text style={styles.small}>sample key {newIdempotencyKey()}</Text>
      <Text style={styles.small}>sample amount {formatMoney(123456, "CAD")}</Text>
      <StatusBar style="auto" />
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: "#fff", alignItems: "center", justifyContent: "center", gap: 8 },
  title: { fontSize: 28, fontWeight: "600" },
  small: { fontSize: 12, color: "#666" },
});
