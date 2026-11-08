import { useEffect, useRef, useState } from "react";

/**
 * Calls `tick` every `intervalMs` while `active` is true, and gives up after `maxMs` (then
 * `gaveUp` is true and the ticks stop; `restart()` starts another round, for pull to refresh).
 * Stops when `active` turns false or the screen unmounts.
 */
export function usePolling(active: boolean, tick: () => void, intervalMs: number, maxMs: number) {
  const tickRef = useRef(tick);
  tickRef.current = tick;
  const [round, setRound] = useState(0);
  const [gaveUp, setGaveUp] = useState(false);

  useEffect(() => {
    setGaveUp(false);
    if (!active) return;
    const started = Date.now();
    const id = setInterval(() => {
      if (Date.now() - started >= maxMs) {
        clearInterval(id);
        setGaveUp(true);
        return;
      }
      tickRef.current();
    }, intervalMs);
    return () => clearInterval(id);
  }, [active, intervalMs, maxMs, round]);

  return { gaveUp, restart: () => setRound((n) => n + 1) };
}
