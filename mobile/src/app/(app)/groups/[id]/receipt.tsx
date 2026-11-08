import * as ImagePicker from "expo-image-picker";
import { router, useLocalSearchParams } from "expo-router";
import { useRef, useState } from "react";
import { ActivityIndicator, Button, Image, ScrollView, StyleSheet, Text, TextInput, View } from "react-native";

import { unwrap } from "../../../../api/client";
import { describeError } from "../../../../api/errors";
import { api } from "../../../../api/instance";
import { keyedByValue } from "../../../../api/keys";
import { receiptForm } from "../../../../api/receipts";
import { prepareReceiptImage, type PreparedImage } from "../../../../receipts/prepare";

export default function AddReceipt() {
  const { id } = useLocalSearchParams<{ id: string }>();
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
    <ScrollView
      contentInsetAdjustmentBehavior="automatic" // start below the translucent iOS header
      keyboardShouldPersistTaps="handled"
      contentContainerStyle={styles.container}
    >
      <Text style={styles.label}>Name (optional)</Text>
      <TextInput
        style={styles.input}
        value={description}
        onChangeText={setDescription}
        placeholder="Receipt"
        maxLength={200}
        editable={!busy}
      />

      {photo !== null && (
        <Image
          source={{ uri: photo.uri }}
          style={[styles.preview, { aspectRatio: photo.width / photo.height }]}
          resizeMode="contain"
          accessibilityLabel="The receipt photo that will be uploaded"
        />
      )}
      {preparing ? (
        <ActivityIndicator />
      ) : (
        <Button title={photo === null ? "Choose a photo" : "Choose another photo"} onPress={choose} disabled={uploading} />
      )}

      {photo !== null && (
        <View style={styles.upload}>
          {uploading ? <ActivityIndicator /> : <Button title="Upload" onPress={upload} disabled={busy} />}
          <Text style={styles.note}>
            The photo is turned upright and shrunk to at most 2400 px before it is sent. Location data is not sent.
          </Text>
        </View>
      )}
      {error !== null && <Text style={styles.error}>{error}</Text>}
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  container: { padding: 24, gap: 12 },
  label: { color: "#444" },
  input: { borderWidth: 1, borderColor: "#bbb", borderRadius: 8, padding: 12, fontSize: 17, backgroundColor: "#fff" },
  preview: { width: "100%", maxHeight: 360, backgroundColor: "#ddd" },
  upload: { gap: 8 },
  note: { color: "#666", fontSize: 12 },
  error: { color: "#b00020" },
});
