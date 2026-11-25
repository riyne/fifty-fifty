import { useLocalSearchParams } from "expo-router";
import { useState } from "react";
import { Share, StyleSheet, View } from "react-native";

import { unwrap } from "../../../../api/client";
import { describeError } from "../../../../api/errors";
import { api } from "../../../../api/instance";
import type { CreatedJoinCode } from "../../../../api/types";
import { Button } from "../../../../ui/Button";
import { radius, space, useColors } from "../../../../ui/theme";
import { Txt } from "../../../../ui/Txt";

// Owner only (the api answers 403 to anyone else). The code is shown once: the api keeps only a hash.
// A code is made on the tap, not when the sheet opens, so looking at the sheet does not use one up.
export default function Invite() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const colors = useColors();
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<CreatedJoinCode | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function create() {
    setBusy(true);
    setError(null);
    try {
      setCreated(await unwrap(api.POST("/v1/groups/{id}/join-codes", { params: { path: { id } } })));
    } catch (e) {
      setError(describeError(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <View style={styles.sheet}>
      <Txt variant="title" accessibilityRole="header">
        Invite people
      </Txt>
      {created === null ? (
        <>
          <Txt tone="secondary">Create a code and send it to them. They enter it under Join with code.</Txt>
          <Button title="Create an invite code" onPress={() => void create()} busy={busy} />
        </>
      ) : (
        <>
          <View style={[styles.code, { backgroundColor: colors.accentSoft }]}>
            <Txt variant="title" tone="accent" selectable numberOfLines={1} adjustsFontSizeToFit style={styles.codeText}>
              {created.code}
            </Txt>
          </View>
          <Txt variant="small" tone="secondary">
            Shown once. Up to {created.max_uses} people can use it until {new Date(created.expires_at).toLocaleString()}.
          </Txt>
          <Button
            title="Share the code"
            icon="square.and.arrow.up"
            onPress={() => void Share.share({ message: `Join my group on Fifty Fifty with this code: ${created.code}` }).catch(() => {})}
          />
        </>
      )}
      {error !== null && <Txt tone="danger">{error}</Txt>}
    </View>
  );
}

const styles = StyleSheet.create({
  sheet: { padding: space.xl, paddingTop: space.xl + space.s, gap: space.m },
  code: { padding: space.l, borderRadius: radius.m, alignItems: "center" },
  codeText: { fontSize: 28, letterSpacing: 2 },
});
