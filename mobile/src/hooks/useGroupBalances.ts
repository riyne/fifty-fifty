import { useEffect, useRef, useState } from "react";

import { unwrap } from "../api/client";
import { api } from "../api/instance";
import { createLatest } from "./latest";

/** Each group costs one request, so only the first groups of the list get a balance. */
export const BALANCE_LIMIT = 20;

/**
 * The signed-in user's balance in each of the first groups, by group id. Loaded after the list is
 * on screen and again whenever `groups` is a new list. A group whose request failed is left out:
 * the list then shows no balance for it rather than an old or wrong one.
 */
export function useGroupBalances(groups: readonly { id: string }[] | null, meId: string | null): ReadonlyMap<string, number> {
  const [balances, setBalances] = useState<ReadonlyMap<string, number>>(new Map());
  const latest = useRef(createLatest()).current;

  useEffect(() => {
    if (groups === null || meId === null) return;
    const ticket = latest.next();
    void Promise.all(
      groups.slice(0, BALANCE_LIMIT).map(async ({ id }) => {
        try {
          const r = await unwrap(api.GET("/v1/groups/{id}/balances", { params: { path: { id } } }));
          const mine = r.balances.find((b) => b.member_id === meId);
          return mine === undefined ? null : ([id, mine.balance_minor] as const);
        } catch {
          return null;
        }
      }),
    ).then((entries) => {
      if (latest.isCurrent(ticket)) setBalances(new Map(entries.filter((entry) => entry !== null)));
    });
    return () => latest.cancel();
  }, [groups, meId, latest]);

  return balances;
}
