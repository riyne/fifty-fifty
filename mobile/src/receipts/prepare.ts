import { ImageManipulator, SaveFormat } from "expo-image-manipulator";

import { fitWithin } from "./size";

export interface PreparedImage {
  uri: string;
  width: number;
  height: number;
  /** Always what the file is: the image is re-encoded as JPEG. */
  mimeType: "image/jpeg";
}

/**
 * Makes the photo the api wants: upright pixels, a JPEG, the long side at most MAX_SIDE_PX.
 *
 * The api removes the EXIF orientation tag without applying it (DESIGN section 13), so a phone
 * photo taken sideways would be stored sideways. Loading the image here applies the
 * orientation, and saving writes a new file whose pixels are upright and which carries no
 * orientation tag. The size comes from the loaded image, not from the picker's report.
 * Call it ONCE per chosen photo and send the same file on a retry: the api compares the
 * request's bytes under an Idempotency-Key, and a second encode need not give the same bytes.
 */
export async function prepareReceiptImage(sourceUri: string): Promise<PreparedImage> {
  const loaded = await ImageManipulator.manipulate(sourceUri).renderAsync();
  const target = fitWithin(loaded.width, loaded.height);
  const ready = target === null ? loaded : await ImageManipulator.manipulate(loaded).resize(target).renderAsync();
  const saved = await ready.saveAsync({ format: SaveFormat.JPEG, compress: 0.9 });
  return { uri: saved.uri, width: saved.width, height: saved.height, mimeType: "image/jpeg" };
}
