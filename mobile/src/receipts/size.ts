/**
 * The longest side a receipt photo is sent with. The api accepts up to 10000 px a side and 25
 * million pixels, and a phone photo can be bigger than that (48 MP); 2400 px keeps small print
 * readable and the upload small. Chosen, not measured: how this affects OCR on real photos is
 * not known.
 */
export const MAX_SIDE_PX = 2400;

/**
 * The size to scale an image to so that its longer side is at most `maxSide`, keeping the
 * aspect ratio; null when it already fits. Integer pixels, never below 1.
 */
export function fitWithin(width: number, height: number, maxSide: number = MAX_SIDE_PX): { width: number; height: number } | null {
  if (![width, height, maxSide].every((n) => Number.isInteger(n) && n > 0)) {
    throw new RangeError(`sizes must be positive integers: ${width}x${height}, max ${maxSide}`);
  }
  const longSide = Math.max(width, height);
  if (longSide <= maxSide) return null;
  const scale = (side: number) => Math.max(1, Math.round((side * maxSide) / longSide));
  return width >= height ? { width: maxSide, height: scale(height) } : { width: scale(width), height: maxSide };
}
