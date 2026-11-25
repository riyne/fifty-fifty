import { Stack } from "expo-router";

import { useColors } from "../../ui/theme";

export default function AppLayout() {
  const colors = useColors();
  // a sheet as tall as what it shows
  const sheet = {
    presentation: "formSheet",
    sheetAllowedDetents: "fitToContents",
    sheetGrabberVisible: true,
    headerShown: false,
    contentStyle: { backgroundColor: colors.card },
  } as const;
  return (
    <Stack
      screenOptions={{
        headerBackButtonDisplayMode: "minimal", // only the arrow: a long previous title pushed this one off centre
        headerTintColor: colors.accent,
        headerTitleStyle: { color: colors.text },
        headerShadowVisible: false,
      }}
    >
      <Stack.Screen name="index" options={{ title: "Groups", headerLargeTitleEnabled: true }} />
      <Stack.Screen name="settings" options={{ title: "Settings", ...sheet }} />
      <Stack.Screen name="groups/new" options={{ title: "New group" }} />
      <Stack.Screen name="join" options={{ title: "Join a group" }} />
      <Stack.Screen name="groups/[id]/index" options={{ title: "" }} />
      <Stack.Screen name="groups/[id]/invite" options={{ title: "Invite", ...sheet }} />
      <Stack.Screen name="groups/[id]/receipt" options={{ title: "Add a receipt" }} />
      <Stack.Screen name="groups/[id]/settle" options={{ title: "Pay" }} />
      <Stack.Screen name="groups/[id]/settlements/[settlementId]" options={{ title: "Payment" }} />
      <Stack.Screen name="groups/[id]/expenses/[expenseId]/index" options={{ title: "" }} />
      <Stack.Screen name="groups/[id]/expenses/[expenseId]/review" options={{ title: "Review items" }} />
    </Stack>
  );
}
