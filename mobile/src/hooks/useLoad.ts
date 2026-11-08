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
 */
export function useLoad<T>(fetcher: () => Promise<T>) {
  const [state, setState] = useState<Load<T>>({ status: "loading" });
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;
  const latest = useRef(createLatest()).current;

  const reload = useCallback(async () => {
    const ticket = latest.next();
    setState((previous) => (previous.status === "ready" ? { ...previous, refreshing: true } : { status: "loading" }));
    try {
      const data = await fetcherRef.current();
      if (latest.isCurrent(ticket)) setState({ status: "ready", data, refreshing: false });
    } catch (error) {
      if (latest.isCurrent(ticket)) setState({ status: "failed", message: describeError(error) });
    }
  }, [latest]);

  useEffect(() => () => latest.cancel(), [latest]);

  return { state, reload };
}
