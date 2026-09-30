import React from "react";

/** The conversation-scoped commands the palette offers, or null on a route with none. */
export interface PaletteContextValue {
  /** Names the surface the commands act on, for the rows' second line. */
  readonly conversationTitle: string | null;
  readonly issueIdentifier: string | null;
  /** Opens the handoff form. Null when this conversation cannot be linked. */
  readonly createIssue: (() => void) | null;
  /** Opens the linked issue in Work. Null when there is none. */
  readonly openIssue: (() => void) | null;
  readonly copyLink: () => void;
  /** Opens the right panel's Diff surface. Null when there is no change to show. */
  readonly openDiff: (() => void) | null;

  readonly toggleRightPanel: () => void;
}

let current: PaletteContextValue | null = null;
const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of listeners) listener();
}

/**
 * Publishes the open conversation's commands. Returns the undo, so the caller
 * can withdraw them on unmount and a palette opened on `/work` does not offer
 * to copy a link to a conversation that is no longer on screen.
 */
export function publishPaletteContext(value: PaletteContextValue): () => void {
  current = value;
  emit();
  return () => {
    if (current === value) {
      current = null;
      emit();
    }
  };
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function snapshot(): PaletteContextValue | null {
  return current;
}

export function usePaletteContext(): PaletteContextValue | null {
  return React.useSyncExternalStore(subscribe, snapshot, snapshot);
}

export interface PaletteShellValue {
  readonly toggleSidebar: () => void;
}

let shell: PaletteShellValue | null = null;
const shellListeners = new Set<() => void>();

export function publishPaletteShell(value: PaletteShellValue): () => void {
  shell = value;
  for (const listener of shellListeners) listener();
  return () => {
    if (shell === value) {
      shell = null;
      for (const listener of shellListeners) listener();
    }
  };
}

function subscribeShell(listener: () => void): () => void {
  shellListeners.add(listener);
  return () => {
    shellListeners.delete(listener);
  };
}

function shellSnapshot(): PaletteShellValue | null {
  return shell;
}

export function usePaletteShell(): PaletteShellValue | null {
  return React.useSyncExternalStore(subscribeShell, shellSnapshot, shellSnapshot);
}
