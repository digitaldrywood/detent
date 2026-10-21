import { create } from "zustand";

import type { ScopedThreadRef } from "./environment/scoped.ts";

export interface ThreadTerminalUiState {
  readonly terminalOpen: boolean;
}

const CLOSED: ThreadTerminalUiState = { terminalOpen: false };

export interface TerminalUiStateStore {
  readonly terminalUiStateByThreadKey: Record<string, ThreadTerminalUiState>;
}

export const useTerminalUiStateStore = create<TerminalUiStateStore>()(() => ({
  terminalUiStateByThreadKey: {},
}));

export function selectThreadTerminalUiState(
  terminalUiStateByThreadKey: Record<string, ThreadTerminalUiState>,
  threadRef: ScopedThreadRef | null | undefined,
): ThreadTerminalUiState {
  if (!threadRef || (threadRef.threadId as string).length === 0) {
    return CLOSED;
  }
  return (
    terminalUiStateByThreadKey[`${threadRef.environmentId}:${threadRef.threadId as string}`] ??
    CLOSED
  );
}
