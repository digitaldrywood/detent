export * from "./app/adapters/rightPanel.ts";

import type { ScopedThreadRef } from "./environment/scoped.ts";

export interface RightPanelActions {
  openFile: (ref: ScopedThreadRef, relativePath: string, line?: number) => void;
  openBrowser: (ref: ScopedThreadRef, tabId: string) => void;
}

const NO_ACTIONS: RightPanelActions = {
  openFile: () => {},
  openBrowser: () => {},
};

let mounted: RightPanelActions | null = null;

/**
 * Registers the mounted panel's actions, and returns the deregistration the
 * caller runs on unmount. Deregistering only clears the slot if it is still
 * this caller's, so a remount that registers before the old one tears down
 * cannot leave the panel unreachable.
 */
export function registerRightPanelActions(actions: RightPanelActions): () => void {
  mounted = actions;
  return () => {
    if (mounted === actions) mounted = null;
  };
}

export const useRightPanelStore = {
  getState: (): RightPanelActions => mounted ?? NO_ACTIONS,
};
