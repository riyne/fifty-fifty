import { File } from "expo-file-system";

/**
 * The multipart body of POST /v1/groups/{id}/receipts: one `file` part and, when the user typed
 * one, a `description` part (the api's default is "Receipt").
 *
 * The file part is an `expo-file-system` File. The app's global fetch is Expo's, and it accepts
 * only strings, Blobs and objects with `bytes()` as form parts: React Native's older
 * `{ uri, name, type }` part is refused ("Unsupported FormDataPart implementation"), which the
 * first run on the simulator showed. A File reads its bytes from the local file when the request
 * is sent and gives the part its name and content type from the file name.
 */
export function receiptForm(file: { uri: string }, description: string): FormData {
  const form = new FormData();
  form.append("file", new File(file.uri));
  const trimmed = description.trim();
  if (trimmed !== "") form.append("description", trimmed);
  return form;
}
