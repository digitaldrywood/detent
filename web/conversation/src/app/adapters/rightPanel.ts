import React from "react";

import type { MessageAttachment } from "../../contracts/conversation.ts";

const RIGHT_PANEL_KINDS = [
  "diff",
  "files",
  "file",
  "preview",
  "terminal",
  "pull-request",

  "conversation",

  "output",
] as const;
export type RightPanelKind = (typeof RIGHT_PANEL_KINDS)[number];

export type RightPanelSurface =
  | { id: `browser:${string}`; kind: "preview"; resourceId: string }
  | { id: "browser:new"; kind: "preview"; resourceId: null }
  | {
      id: `terminal:${string}`;
      kind: "terminal";
      resourceId: string;
      terminalIds: string[];
      activeTerminalId: string;
      splitDirection?: "horizontal" | "vertical";
    }
  | { id: "diff"; kind: "diff" }
  | { id: "files"; kind: "files" }
  | {
      id: `file:${string}` | `attachment:${string}`;
      kind: "file";
      /** Workspace-relative, or absolute for a host file outside the workspace. */
      relativePath: string;
      revealLine: number | null;
      revealRequestId: number;
      attachment?: MessageAttachment;
    }
  | {
      id: `pull-request:${string}`;
      kind: "pull-request";
      environmentId?: string;
      projectId: string;
      repository: string;
      number: number;
      url?: string;
    }
  | { id: `conversation:${string}`; kind: "conversation"; conversationId: string }
  | { id: "output"; kind: "output" };

export interface RightPanelState {
  readonly isOpen: boolean;
  readonly maximized: boolean;
  readonly activeSurfaceId: string | null;
  readonly surfaces: readonly RightPanelSurface[];
}

const EMPTY: RightPanelState = {
  isOpen: false,
  maximized: false,
  activeSurfaceId: null,
  surfaces: [],
};

const STORAGE_KEY = "detent:right-panel-state:v1";

type Stored = Record<string, RightPanelState>;

function readStored(): Stored {
  try {
    const raw = globalThis.localStorage?.getItem(STORAGE_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    return typeof parsed === "object" && parsed !== null ? (parsed as Stored) : {};
  } catch {
    return {};
  }
}

function writeStored(value: Stored): void {
  try {
    globalThis.localStorage?.setItem(STORAGE_KEY, JSON.stringify(value));
  } catch {
    // A private window with storage denied still gets a working panel; it just
    // does not remember which surface was open.
  }
}

export interface RightPanelController extends RightPanelState {
  readonly open: () => void;
  readonly close: () => void;
  readonly toggle: () => void;
  readonly toggleMaximized: () => void;
  readonly activate: (surface: RightPanelSurface) => void;
  readonly addSurface: (surface: RightPanelSurface) => void;
  readonly closeSurface: (surface: RightPanelSurface) => void;
  readonly closeOthers: (surface: RightPanelSurface) => void;
  readonly closeToRight: (surface: RightPanelSurface) => void;
  readonly closeAll: () => void;
}

export function useRightPanel(scopeKey: string): RightPanelController {
  const [state, setState] = React.useState<RightPanelState>(() => readStored()[scopeKey] ?? EMPTY);

  // A scope change re-reads rather than resets: coming back to a conversation
  // finds the surface that was open on it.
  const previousScope = React.useRef(scopeKey);
  if (previousScope.current !== scopeKey) {
    previousScope.current = scopeKey;
    // eslint-disable-next-line react-hooks/rules-of-hooks
  }
  React.useEffect(() => {
    setState(readStored()[scopeKey] ?? EMPTY);
  }, [scopeKey]);

  const update = React.useCallback(
    (next: (current: RightPanelState) => RightPanelState) => {
      setState((current) => {
        const value = next(current);
        const stored = readStored();
        stored[scopeKey] = value;
        writeStored(stored);
        return value;
      });
    },
    [scopeKey],
  );

  return React.useMemo<RightPanelController>(() => {
    const withSurface = (
      current: RightPanelState,
      surface: RightPanelSurface,
    ): RightPanelState => {
      const existing = current.surfaces.find((entry) => entry.id === surface.id);
      const surfaces = existing === undefined ? [...current.surfaces, surface] : current.surfaces;
      return { ...current, isOpen: true, surfaces, activeSurfaceId: surface.id };
    };
    const without = (current: RightPanelState, ids: ReadonlySet<string>): RightPanelState => {
      const surfaces = current.surfaces.filter((entry) => !ids.has(entry.id));
      const activeSurfaceId =
        current.activeSurfaceId !== null && ids.has(current.activeSurfaceId)
          ? (surfaces.at(-1)?.id ?? null)
          : current.activeSurfaceId;
      return { ...current, surfaces, activeSurfaceId };
    };
    return {
      ...state,
      open: () => update((current) => ({ ...current, isOpen: true })),
      close: () => update((current) => ({ ...current, isOpen: false })),
      toggle: () => update((current) => ({ ...current, isOpen: !current.isOpen })),
      toggleMaximized: () => update((current) => ({ ...current, maximized: !current.maximized })),
      activate: (surface) =>
        update((current) => ({ ...current, isOpen: true, activeSurfaceId: surface.id })),
      addSurface: (surface) => update((current) => withSurface(current, surface)),
      closeSurface: (surface) => update((current) => without(current, new Set([surface.id]))),
      closeOthers: (surface) =>
        update((current) =>
          without(
            current,
            new Set(
              current.surfaces
                .filter((entry) => entry.id !== surface.id)
                .map((entry) => entry.id),
            ),
          ),
        ),
      closeToRight: (surface) =>
        update((current) => {
          const index = current.surfaces.findIndex((entry) => entry.id === surface.id);
          if (index < 0) return current;
          return without(
            current,
            new Set(current.surfaces.slice(index + 1).map((entry) => entry.id)),
          );
        }),
      closeAll: () =>
        update((current) => ({ ...current, surfaces: [], activeSurfaceId: null })),
    };
  }, [state, update]);
}
