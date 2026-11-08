import { useCallback, useEffect, useRef, useState } from "react";

import { describeError } from "../api/errors";
import { createLatest } from "./latest";

export type Load<T> =
  | { status: "loading" }
  | { status: "ready"; data: T; refreshing: boolean }
  | { status: "failed"; message: string };

/**
 * Loads data with `fetcher` and gives `reload` to load it again (pull to refresh, coming back
 * to the screen). While reloading, the old data stays on screen. A failed load replaces it
 * with the error. Answers that arrive late or after unmount are dropped.
 *
 * `reload({ silent: true })` is for polling: it does not show the refresh spinner, and a failed
 * attempt keeps the data on screen (the next tick tries again) instead of replacing it with an error.
 */
export function useLoad<T>(fetcher: () => Promise<T>) {
  const [state, setState] = useState<Load<T>>({ status: "loading" });
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const latest = useRef(createLatest()).current;

  const reload = useCallback(
    async (options?: { silent?: boolean }) => {
      const silent = options?.silent === true;
      const ticket = latest.next();
      if (!silent) {
        setState((previous) => (previous.status === "ready" ? { ...previous, refreshing: true } : { status: "loading" }));
      }
      try {
        const data = await fetcherRef.current();
        if (latest.isCurrent(ticket)) setState({ status: "ready", data, refreshing: false });
      } catch (error) {
        if (!latest.isCurrent(ticket)) return;
        const message = describeError(error);
        setState((previous) => (silent && previous.status === "ready" ? previous : { status: "failed", message }));
      }
    },
    [latest],
  );

  useEffect(() => () => latest.cancel(), [latest]);

  return { state, reload };
}
