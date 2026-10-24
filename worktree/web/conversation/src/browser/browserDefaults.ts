import {
  DEFAULT_BROWSER_PROFILE_ID,
  FILL_PREVIEW_VIEWPORT,
  type BrowserProfileId,
  type PreviewViewportSetting,
} from "@t3tools/contracts";

export interface BrowserProfile {
  readonly id: string;
  readonly name: string;
}

export interface BrowserDefaults {
  readonly viewport: PreviewViewportSetting;
  readonly profiles: readonly BrowserProfile[];
  readonly profileId: BrowserProfileId;
}

const BROWSER_DEFAULTS: BrowserDefaults = {
  viewport: FILL_PREVIEW_VIEWPORT,
  profiles: [],
  profileId: DEFAULT_BROWSER_PROFILE_ID,
};

export function useBrowserDefaults(): BrowserDefaults {
  return BROWSER_DEFAULTS;
}

/**
 * The defaults, once client settings have actually loaded.
 *
 * Upstream this awaits hydration because a tab opened before the reader's saved
 * settings arrive would be born at the wrong viewport, zoom and profile and
 * never corrected. Nothing here is stored or loaded, so it answers at once —
 * the async shape is upstream's and the copied `openFileInPreview.ts` awaits
 * it, including the rejection path it maps to `BrowserSettingsReadError`.
 */
export function resolveBrowserDefaults(): Promise<BrowserDefaults> {
  return Promise.resolve(BROWSER_DEFAULTS);
}

/**
 * The viewport a *newly opened* tab should start at. Sent with `preview.open`
 * so the session is born at the configured size instead of being resized a
 * frame later, which the user would see as a visible reflow.
 */
export function browserDefaultOpenViewport(
  defaults: BrowserDefaults = BROWSER_DEFAULTS,
): PreviewViewportSetting {
  return defaults.viewport;
}

/** Profile a tab opens under when the caller doesn't name one. */
export function browserDefaultOpenProfileId(
  defaults: BrowserDefaults = BROWSER_DEFAULTS,
): BrowserProfileId {
  return defaults.profileId;
}
