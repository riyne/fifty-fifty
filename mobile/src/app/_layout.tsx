import { Stack } from "expo-router";
import { StatusBar } from "expo-status-bar";
import { useEffect } from "react";
import { ActivityIndicator, View } from "react-native";

import { session, useSession } from "../auth";

export default function RootLayout() {
  const state = useSession();

  useEffect(() => {
    void session.restore(); // reads the Keychain once; the state leaves "loading"
  }, []);

  if (state.status === "loading") {
    return (
      <View style={{ flex: 1, alignItems: "center", justifyContent: "center" }}>
        <ActivityIndicator />
      </View>
    );
  }

  const signedIn = state.status === "signedIn";
  return (
    <>
      <StatusBar style="auto" />
      <Stack screenOptions={{ headerShown: false }}>
        <Stack.Protected guard={!signedIn}>
          <Stack.Screen name="login" options={{ headerShown: true, title: "Sign in" }} />
        </Stack.Protected>
        {/* Every signed-in screen lives under (app), so a new one is guarded without listing it. */}
        <Stack.Protected guard={signedIn} redirectTo="/login">
          <Stack.Screen name="(app)" />
        </Stack.Protected>
      </Stack>
    </>
  );
}
