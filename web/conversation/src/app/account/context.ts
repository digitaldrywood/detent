// What every account screen needs: the extended bootstrap payload and one
// API client built from it.
//
// The client is memoised on the conversation client, so the screens share a
// single instance and a CSRF token that came from the same payload the shell
// loaded (decisions.md §12, "Serving").
import React from "react";

import type { AccountBootstrap } from "../../contracts/account.ts";
import { useClient } from "../client.ts";
import { makeAccountApi, type AccountApi } from "./api.ts";

/**
 * The extended bootstrap, or `null` against a hub that only serves the chat
 * payload. Every screen treats `null` as "this hub cannot answer that yet"
 * rather than inventing an actor.
 */
export function useAccountBootstrap(): AccountBootstrap | null {
  const client = useClient();
  return client.account ?? null;
}

const CACHE = new WeakMap<object, AccountApi>();

export function useAccountApi(): AccountApi {
  const client = useClient();
  return React.useMemo(() => {
    const cached = CACHE.get(client as unknown as object);
    if (cached !== undefined) return cached;
    const api = makeAccountApi({
      origin: client.http.origin,
      apiBase: client.http.apiBase,
      csrfToken: client.http.csrfToken,
    });
    CACHE.set(client as unknown as object, api);
    return api;
  }, [client]);
}

/** True when the actor may run the organization's management endpoints. */
export function useCanManage(): boolean {
  return useAccountBootstrap()?.actor.can_manage ?? false;
}
