import * as ImagePicker from "expo-image-picker";
import { router, useLocalSearchParams } from "expo-router";
import { useRef, useState } from "react";
import { ActivityIndicator, Image, Pressable, ScrollView, StyleSheet } from "react-native";

import { unwrap } from "../../../../api/client";
import { describeError } from "../../../../api/errors";
import { api } from "../../../../api/instance";
import { keyedByValue } from "../../../../api/keys";
import { receiptForm } from "../../../../api/receipts";
import { prepareReceiptImage, type PreparedImage } from "../../../../receipts/prepare";
import { Button } from "../../../../ui/Button";
import { ErrorBar } from "../../../../ui/ErrorBar";
import { Field } from "../../../../ui/Field";
import { Footer } from "../../../../ui/Footer";
import { Icon } from "../../../../ui/Icon";
import { radius, space, useColors } from "../../../../ui/theme";
import { Txt } from "../../../../ui/Txt";

export default function AddReceipt() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const colors = useColors();
  const [photo, setPhoto] = useState<PreparedImage | null>(null);
  const [description, setDescription] = useState("");
  const [preparing, setPreparing] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The same photo and description tapped again after a lost answer is a retry (same key); the
  // photo is prepared once when it is chosen, so a retry sends the same bytes. A different photo
  // or description is a different request and gets a new key.
  const keyFor = useRef(keyedByValue()).current;

  async function choose() {
    setError(null);
    try {
      // The system photo picker runs outside the app, so no photo-library permission is asked.
      const picked = await ImagePicker.launchImageLibraryAsync({ mediaTypes: ["images"], allowsEditing: false, quality: 1 });
      if (picked.canceled) return;
      setPreparing(true);
      setPhoto(await prepareReceiptImage(picked.assets[0].uri));
    } catch {
      setError("Couldn't read that photo. Try another one.");
    } finally {
      setPreparing(false);
    }
  }

  async function upload() {
    if (photo === null) return;
    setUploading(true);
    setError(null);
    try {
      const accepted = await unwrap(
        api.POST("/v1/groups/{id}/receipts", {
          params: {
            path: { id },
            header: { "Idempotency-Key": keyFor(`${photo.uri}\n${description.trim()}`) },
          },
          body: { file: photo.uri }, // only for the types; the real body is the form below
          bodySerializer: () => receiptForm(photo, description),
        }),
      );
      router.replace({ pathname: "/groups/[id]/expenses/[expenseId]", params: { id, expenseId: accepted.expense_id } });
    } catch (e) {
      setError(
        describeError(e, {
          413: "That photo is too large. Try a smaller one.",
          415: "The server did not accept that kind of file.",
        }),
      );
      setUploading(false);
    }
  }

  const busy = preparing || uploading;
  return (
    <>
      <ScrollView
        contentInsetAdjustmentBehavior="automatic" // start below the translucent iOS header
        automaticallyAdjustKeyboardInsets
        keyboardDismissMode="on-drag"
        keyboardShouldPersistTaps="handled"
        contentContainerStyle={styles.container}
      >
        <Field
          label="Name (optional)"
          value={description}
          onChangeText={setDescription}
          placeholder="Thai dinner"
          maxLength={200}
          returnKeyType="done"
          editable={!busy}
        />
        {photo === null ? (
          <Pressable
            accessibilityRole="button"
            accessibilityLabel="Choose a photo of the receipt"
            onPress={() => void choose()}
            disabled={busy}
            style={({ pressed }) => [styles.drop, { backgroundColor: pressed ? colors.pressed : colors.card, borderColor: colors.secondary }]}
          >
            {preparing ? (
              <ActivityIndicator />
            ) : (
              <>
                <Icon name="doc.text.viewfinder" size={40} tone="accent" />
                <Txt variant="heading">Choose a photo</Txt>
                <Txt variant="small" tone="secondary" style={styles.centered}>
                  The whole receipt, flat and in focus, reads best.
                </Txt>
              </>
            )}
          </Pressable>
        ) : (
          <>
            <Image
              source={{ uri: photo.uri }}
              style={[styles.preview, { aspectRatio: photo.width / photo.height, backgroundColor: colors.card }]}
              resizeMode="contain"
              accessibilityLabel="The receipt photo that will be uploaded"
            />
            <Button kind="secondary" title="Choose another photo" onPress={() => void choose()} busy={preparing} disabled={uploading} />
            <Txt variant="small" tone="secondary" style={styles.centered}>
              The photo is turned upright and shrunk to at most 2400 px before it is sent. Location data is not sent.
            </Txt>
          </>
        )}
      </ScrollView>
      {error !== null && <ErrorBar text={error} />}
      <Footer>
        <Button grow title="Upload" onPress={() => void upload()} busy={uploading} disabled={photo === null || preparing} />
      </Footer>
    </>
  );
}

const styles = StyleSheet.create({
  container: { padding: space.l, gap: space.l },
  drop: { minHeight: 240, alignItems: "center", justifyContent: "center", gap: space.s, padding: space.xl, borderRadius: radius.m, borderWidth: 1, borderStyle: "dashed" },
  centered: { textAlign: "center" },
  preview: { width: "100%", maxHeight: 420, borderRadius: radius.s },
});
