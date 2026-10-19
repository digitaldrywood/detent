// Settings consumed by shared components, backed by Detent client preferences.
import React from "react";
import * as Schema from "effect/Schema";

import { getLocalStorageItem, setLocalStorageItem } from "../../hooks/useLocalStorage.ts";
import { DEFAULT_BROWSER_LINK_TARGET, type BrowserLinkTarget } from "../../contracts/ui.ts";

export const MIN_PANEL_ANIMATION_DURATION_MS = 0;
export const MAX_PANEL_ANIMATION_DURATION_MS = 1_000;
export const DEFAULT_PANEL_ANIMATION_DURATION_MS = 0;

export const PANEL_ANIMATION_DURATION_STORAGE_KEY = "panel_animation_duration_ms";

export type PanelAnimationDurationMs = number;

export type TimestampFormat = "locale" | "12-hour" | "24-hour";
export const DEFAULT_TIMESTAMP_FORMAT: TimestampFormat = "locale";

export type SidebarProjectSortOrder = "updated_at" | "created_at" | "manual";
export const DEFAULT_SIDEBAR_PROJECT_SORT_ORDER: SidebarProjectSortOrder = "manual";

export type SidebarProjectGroupingMode = "repository" | "repository_path" | "separate";
export const DEFAULT_SIDEBAR_PROJECT_GROUPING_MODE: SidebarProjectGroupingMode = "separate";

export interface ClientSettings {
  readonly panelAnimationDurationMs: PanelAnimationDurationMs;
  readonly timestampFormat: TimestampFormat;
  readonly sidebarProjectSortOrder: SidebarProjectSortOrder;
  readonly sidebarProjectGroupingMode: SidebarProjectGroupingMode;
  readonly sidebarProjectGroupingOverrides: Record<string, SidebarProjectGroupingMode>;

  readonly confirmThreadDelete: boolean;
  readonly confirmThreadArchive: boolean;

  readonly browserLinkTarget: BrowserLinkTarget;

  readonly wordWrap: boolean;
}

/** The one settings document there is, built once. */
function detentClientSettings(): ClientSettings {
  return {
    panelAnimationDurationMs: panelAnimationDurationMs(),
    timestampFormat: DEFAULT_TIMESTAMP_FORMAT,
    sidebarProjectSortOrder: DEFAULT_SIDEBAR_PROJECT_SORT_ORDER,
    sidebarProjectGroupingMode: DEFAULT_SIDEBAR_PROJECT_GROUPING_MODE,
    sidebarProjectGroupingOverrides: {},
    confirmThreadDelete: false,
    confirmThreadArchive: false,
    browserLinkTarget: DEFAULT_BROWSER_LINK_TARGET,
    wordWrap: false,
  };
}

export function getClientSettings(): ClientSettings {
  return detentClientSettings();
}

export function panelAnimationDurationMs(): number {
  let stored: number | null = null;
  try {
    stored = getLocalStorageItem(PANEL_ANIMATION_DURATION_STORAGE_KEY, Schema.Finite);
  } catch {
    stored = null;
  }
  if (stored === null) return DEFAULT_PANEL_ANIMATION_DURATION_MS;
  return Math.min(
    MAX_PANEL_ANIMATION_DURATION_MS,
    Math.max(MIN_PANEL_ANIMATION_DURATION_MS, Math.round(stored)),
  );
}

/** Sets it, for the one place a reader can change it. */
export function setPanelAnimationDurationMs(durationMs: number): void {
  setLocalStorageItem(PANEL_ANIMATION_DURATION_STORAGE_KEY, durationMs, Schema.Finite);
}

export function useClientSettings<T>(select: (settings: ClientSettings) => T): T {
  const [settings] = React.useState<ClientSettings>(detentClientSettings);
  return select(settings);
}

export function ensureClientSettingsHydrated(): Promise<void> {
  return Promise.resolve();
}

export function useEnvironmentIdentificationMode(): "name" | "artwork" | "pill" {
  return "name";
}

export function useLegacySidebarEnabled(): boolean {
  return false;
}

export type SidebarThreadSortOrder = "updated_at" | "created_at";
export const DEFAULT_SIDEBAR_THREAD_SORT_ORDER: SidebarThreadSortOrder = "updated_at";
