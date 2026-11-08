/**
 * "Only the newest request counts": `next()` starts one and returns its ticket, `isCurrent`
 * says whether a ticket is still the newest, and `cancel()` makes every ticket stale (for
 * unmount). A slow answer to an old request must not overwrite a newer one.
 */
export function createLatest() {
  let current = 0;
  return {
    next: (): number => ++current,
    isCurrent: (ticket: number): boolean => ticket === current,
    cancel: (): void => {
      current++;
    },
  };
}
