import { Stack } from "expo-router";

export default function AppLayout() {
  return (
    <Stack>
      <Stack.Screen name="index" options={{ title: "Groups" }} />
      <Stack.Screen name="groups/new" options={{ title: "New group" }} />
      <Stack.Screen name="join" options={{ title: "Join a group" }} />
      <Stack.Screen name="groups/[id]/index" options={{ title: "Group" }} />
      <Stack.Screen name="groups/[id]/expenses/[expenseId]" options={{ title: "Expense" }} />
    </Stack>
  );
}
