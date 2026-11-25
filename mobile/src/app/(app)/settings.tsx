import { router, useLocalSearchParams } from "expo-router";
import { useEffect, useRef, useState } from "react";
import { StyleSheet, View } from "react-native";

import { chooseAppearance, storedAppearance } from "../../appearance";
import { APPEARANCES, type AppearanceChoice } from "../../appearance/choice";
import { session } from "../../auth";
import { Button } from "../../ui/Button";
import { Choice } from "../../ui/Choice";
import { space } from "../../ui/theme";
import { Txt } from "../../ui/Txt";

export default function Settings() {
  const { name } = useLocalSearchParams<{ name: string }>();
  const [appearance, setAppearance] = useState<AppearanceChoice>(storedAppearance);

  function choose(choice: AppearanceChoice) {
    setAppearance(choice);
    chooseAppearance(choice);
  }

  // The sheet closes first and the session ends once it is gone: signing out removes every screen
  // here, and a close still on its way would then find nothing to go back to.
  const leaving = useRef(false);
  useEffect(
    () => () => {
      if (leaving.current) void session.signOut();
    },
    [],
  );

  function signOut() {
    if (leaving.current) return;
    leaving.current = true;
    router.back();
  }

  return (
    <View style={styles.sheet}>
      <View>
        <Txt variant="title" accessibilityRole="header">
          Settings
        </Txt>
        <Txt tone="secondary">Signed in as {name}</Txt>
      </View>
      <Choice label="Appearance" options={APPEARANCES} value={appearance} onChange={choose} />
      <Button kind="danger" title="Sign out" onPress={signOut} />
    </View>
  );
}

const styles = StyleSheet.create({
  sheet: { padding: space.xl, paddingTop: space.xl + space.s, gap: space.l },
});
