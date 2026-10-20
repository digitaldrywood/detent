// The client instance, shared through React context so components never
// construct a second one (which would open a second connection).
import React from "react";

import { lastAccountBootstrap, loadBootstrap, type ConversationClient } from "../runtime/bootstrap.ts";

export const ClientContext = React.createContext<ConversationClient | null>(null);
const RefreshBootstrapContext = React.createContext<(() => Promise<void>) | null>(null);

export function ClientProvider({ client: initialClient, children }: {
  readonly client: ConversationClient;
  readonly children: React.ReactNode;
}): React.ReactElement {
  const [client, setClient] = React.useState(initialClient);
  const refresh = React.useCallback(async () => {
    const bootstrap = await loadBootstrap(initialClient.http.origin);
    const account = lastAccountBootstrap();
    setClient((current) => ({ ...current, bootstrap, account }));
  }, [initialClient]);
  return React.createElement(RefreshBootstrapContext.Provider, { value: refresh },
    React.createElement(ClientContext.Provider, { value: client }, children));
}

export function useRefreshBootstrap(): () => Promise<void> {
  const refresh = React.useContext(RefreshBootstrapContext);
  return React.useCallback(async () => {
    if (refresh === null) throw new Error("The bootstrap provider is not mounted.");
    await refresh();
  }, [refresh]);
}

export function useClient(): ConversationClient {
  const client = React.useContext(ClientContext);
  if (client === null) throw new Error("The conversation client is not mounted.");
  return client;
}

export { LAST_CHAT_PROJECT_STORAGE_KEY, LAST_PROJECT_STORAGE_KEY } from "../runtime/state/drafts.ts";
import { LAST_CHAT_PROJECT_STORAGE_KEY, LAST_PROJECT_STORAGE_KEY } from "../runtime/state/drafts.ts";

export function readLastProject(): string | null {
  try {
    return globalThis.localStorage?.getItem(LAST_PROJECT_STORAGE_KEY) ?? null;
  } catch {
    return null;
  }
}

export function readLastChatProject(): string | null {
  try {
    const storage = globalThis.localStorage;
    return storage?.getItem(LAST_CHAT_PROJECT_STORAGE_KEY) || storage?.getItem(LAST_PROJECT_STORAGE_KEY) || null;
  } catch {
    return null;
  }
}

export function writeLastProject(projectId: string): void {
  try {
    globalThis.localStorage?.setItem(LAST_PROJECT_STORAGE_KEY, projectId);
    if (projectId !== "") globalThis.localStorage?.setItem(LAST_CHAT_PROJECT_STORAGE_KEY, projectId);
  } catch {
    // A blocked store only costs the preselection.
  }
}

/** Command keys are idempotency keys: one per intent, reused by every retry. */
export function newCommandKey(): string {
  const random = globalThis.crypto?.randomUUID?.();
  return `cmd_${random ?? `${Date.now()}_${Math.random().toString(16).slice(2)}`}`;
}

export function accountKey(client: ConversationClient): string {
  return `${client.bootstrap.organization.id}:${client.bootstrap.actor.principal_id}`;
}
