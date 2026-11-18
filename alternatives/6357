export interface DesktopPreviewOverlay {
  readonly audible?: boolean;
  readonly audioMuted?: boolean;
  readonly favicon?: { readonly pageUrl: string; readonly dataUrl: string } | null;
}

/** Nothing to overlay in a browser client. */
export const EMPTY_DESKTOP_PREVIEW_OVERLAYS: Readonly<
  Record<string, DesktopPreviewOverlay>
> = {};

import type { ScopedThreadRef } from "./environment/scoped.ts";
import type { PreviewSessionSnapshot } from "./contracts/ui.ts";

/**
 * Whether this client can open a browser surface at all. Upstream this asks
 * whether the Electron bridge exposes its preview manager; here the answer is
 * settled by the architecture, not by the host.
 */
export function isPreviewSupportedInRuntime(): boolean {
  return false;
}

/**
 * Upstream: fold a tab's server-side snapshot into the thread's preview state
 * so the panel redraws. Nothing here holds that state, and no path in this
 * client produces a snapshot to fold, so this is where the call stops.
 */
export function applyPreviewServerSnapshot(
  _ref: ScopedThreadRef,
  _snapshot: PreviewSessionSnapshot | null,
): void {}

/**
 * Upstream: push a URL onto the thread's recently-seen ring, which feeds the
 * address bar's suggestions. There is no address bar to suggest into.
 */
export function rememberPreviewUrl(_ref: ScopedThreadRef, _url: string): void {}
