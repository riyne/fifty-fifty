import { useCallback, useEffect, useState } from "react";
import { ActivityIndicator, Button, StyleSheet, Text, View } from "react-native";

import { api } from "../api/instance";
import { unwrap } from "../api/client";
import { ApiError } from "../api/errors";
import { session } from "../auth";

type Me = { id: string; email: string; display_name: string };
type Load = { status: "loading" } | { status: "ready"; me: Me } | { status: "failed"; message: string };

// Step 8.2: proves the login. Replaced by the group list in 8.3.
export default function Home() {
  const [load, setLoad] = useState<Load>({ status: "loading" });

  const fetchMe = useCallback(() => {
    setLoad({ status: "loading" });
    unwrap(api.GET("/v1/me"))
      .then((me) => setLoad({ status: "ready", me }))
      .catch((e: unknown) =>
        // a 401 has already ended the session (the layout shows the login screen)
        setLoad({
          status: "failed",
          message: e instanceof ApiError ? `${e.code}: ${e.message}` : e instanceof Error ? e.message : "failed",
        }),
      );
  }, []);

  useEffect(fetchMe, [fetchMe]);

  return (
    <View style={styles.container}>
      {load.status === "loading" && <ActivityIndicator />}
      {load.status === "ready" && (
        <>
          <Text style={styles.title}>{load.me.display_name}</Text>
          <Text>{load.me.email}</Text>
          <Text style={styles.small}>{load.me.id}</Text>
        </>
      )}
      {load.status === "failed" && (
        <>
          <Text style={styles.error}>{load.message}</Text>
          <Button title="Try again" onPress={fetchMe} />
        </>
      )}
      <Button title="Sign out" onPress={() => void session.signOut()} />
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, alignItems: "center", justifyContent: "center", gap: 12, padding: 24 },
  title: { fontSize: 24, fontWeight: "600" },
  small: { fontSize: 12, color: "#666" },
  error: { color: "#b00020", textAlign: "center" },
});
