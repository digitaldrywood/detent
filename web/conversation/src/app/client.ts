// The client instance, shared through React context so components never
// construct a second one (which would open a second connection).
import React from "react";

import type { ConversationClient } from "../runtime/bootstrap.ts";

export const ClientContext = React.createContext<ConversationClient | null>(null);

export function useClient(): ConversationClient {
  const client = React.useContext(ClientContext);
  if (client === null) throw new Error("The conversation client is not mounted.");
  return client;
}

export { LAST_PROJECT_STORAGE_KEY } from "../runtime/state/drafts.ts";
import { LAST_PROJECT_STORAGE_KEY } from "../runtime/state/drafts.ts";

export function readLastProject(): string | null {
  try {
    return globalThis.localStorage?.getItem(LAST_PROJECT_STORAGE_KEY) ?? null;
  } catch {
    return null;
  }
}

export function writeLastProject(projectId: string): void {
  try {
    globalThis.localStorage?.setItem(LAST_PROJECT_STORAGE_KEY, projectId);
  } catch {
    // A blocked store only costs the preselection.
  }
}

/** Command keys are idempotency keys: one per intent, reused by every retry. */
export function newCommandKey(): string {
  const random = globalThis.crypto?.randomUUID?.();
  return `cmd_${random ?? `${Date.now()}_${Math.random().toString(16).slice(2)}`}`;
}
