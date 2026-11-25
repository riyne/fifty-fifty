import { DarkTheme, DefaultTheme, Stack, ThemeProvider } from "expo-router";
import { StatusBar } from "expo-status-bar";
import { useEffect } from "react";
import { ActivityIndicator, useColorScheme, View } from "react-native";

import { applyStoredAppearance } from "../appearance";
import { session, useSession } from "../auth";
import { useColors } from "../ui/theme";

applyStoredAppearance(); // once, before the first screen is drawn

export default function RootLayout() {
  const state = useSession();
  const colors = useColors();
  const base = useColorScheme() === "dark" ? DarkTheme : DefaultTheme;

  useEffect(() => {
    void session.restore(); // reads the Keychain once; the state leaves "loading"
  }, []);

  if (state.status === "loading") {
    return (
      <View style={{ flex: 1, alignItems: "center", justifyContent: "center", backgroundColor: colors.background }}>
        <ActivityIndicator />
      </View>
    );
  }

  const signedIn = state.status === "signedIn";
  const navigation = {
    ...base,
    colors: { ...base.colors, primary: colors.accent, background: colors.background, card: colors.background, text: colors.text, border: colors.hairline },
  };
  return (
    <ThemeProvider value={navigation}>
      <StatusBar style="auto" />
      <Stack screenOptions={{ headerShown: false }}>
        <Stack.Protected guard={!signedIn}>
          <Stack.Screen name="login" options={{ animation: "fade" }} />
        </Stack.Protected>
        {/* Every signed-in screen lives under (app), so a new one is guarded without listing it. */}
        <Stack.Protected guard={signedIn} redirectTo="/login">
          <Stack.Screen name="(app)" options={{ animation: "fade" }} />
        </Stack.Protected>
      </Stack>
    </ThemeProvider>
  );
}
