// Catalog id → specimens, plus the foundation pages.
//
// The catalog (`src/design-system/catalog.json`) decides what components
// exist; this map only says how to render them. The foundation pages
// (`foundations-*`) are not components and have no catalog entry. Every `available` catalog entry needs a key here,
// either specimens or an `excluded` reason — `tests/designSystemGallery.test.tsx`
// enforces that.
import { normalizeCatalog, rawCatalog, type CatalogEntry } from "./catalog";
import type { GalleryDoc } from "./specimen";
import * as actions from "./specimens/actions";
import * as chat from "./specimens/chat";
import * as feedback from "./specimens/feedback";
import { FOUNDATIONS } from "./specimens/foundations";
import * as forms from "./specimens/forms";
import * as layout from "./specimens/layout";
import * as navigation from "./specimens/navigation";
import * as overlays from "./specimens/overlays";
import { COMPOSITIONS_A } from "./specimens/compositionsA";
import { APP_ROUTES } from "./specimens/appRoutes";
import { COMPOSITIONS_B } from "./specimens/compositionsB";
import { FIELDS_LAYOUT_ENTRY } from "./specimens/fieldsLayoutEntry";
import { SHARED } from "./specimens/shared";
import * as workspace from "./specimens/workspace";
import { activityPage } from "./specimens/activity";

export const REGISTRY: Readonly<Record<string, GalleryDoc>> = {
  // Foundations (not catalog entries: token, type, icon and value rules)
  ...FOUNDATIONS,
  // Actions
  button: actions.button,
  toggle: actions.toggle,
  "toggle-group": actions.toggleGroup,
  "panel-tab-close-button": actions.panelTabCloseButton,
  "refresh-icon": actions.refreshIcon,
  // Forms
  input: forms.input,
  textarea: forms.textarea,
  label: forms.label,
  checkbox: forms.checkbox,
  switch: forms.switchDoc,
  select: forms.select,
  combobox: forms.combobox,
  autocomplete: forms.autocomplete,
  // Overlays
  dialog: overlays.dialog,
  "alert-dialog": overlays.alertDialog,
  sheet: overlays.sheet,
  popover: overlays.popover,
  menu: overlays.menu,
  tooltip: overlays.tooltip,
  "preview-card": overlays.previewCard,
  // Feedback
  alert: feedback.alert,
  badge: feedback.badge,
  toast: feedback.toast,
  spinner: feedback.spinner,
  skeleton: feedback.skeleton,
  empty: feedback.empty,
  "runner-status-dot": feedback.runnerStatusDot,
  "stage-progress": feedback.stageProgress,
  "activity-page": activityPage,
  // Layout and data display
  "scroll-area": layout.scrollArea,
  separator: layout.separator,
  group: layout.group,
  collapsible: layout.collapsible,
  table: layout.table,
  kbd: layout.kbd,
  // Navigation
  sidebar: navigation.sidebar,
  command: navigation.command,
  ...FIELDS_LAYOUT_ENTRY,
  // Compositions
  "app-sidebar-layout": chat.appSidebar,
  composer: chat.composer,
  "conversation-timeline": chat.timeline,
  "chat-markdown": chat.markdown,
  "command-palette-content": chat.commandPalette,
  "right-panel-tabs": workspace.rightPanel,
  "output-surface": workspace.outputSurface,
  "workspace-status-view": workspace.workspaceStatusView,
  "settings-layout": workspace.settingsRows,
  "board-lane": workspace.boardLane,
  "issue-card": workspace.issueCard,
  "usage-page": workspace.usage,
  ...COMPOSITIONS_A,
  ...COMPOSITIONS_B,
  ...APP_ROUTES,
  ...SHARED,
};

/**
 * The entries the gallery lists: the catalog's, in its words, plus any
 * registry entry the catalog does not name yet (shown with its own meta, so a
 * specimen is never hidden by a lagging catalog).
 */
export function galleryEntries(catalog: readonly CatalogEntry[] = normalizeCatalog(rawCatalog())): CatalogEntry[] {
  const seen = new Set(catalog.map((entry) => entry.id));
  const extra: CatalogEntry[] = Object.entries(REGISTRY)
    .filter(([id]) => !seen.has(id))
    .map(([id, doc]) => ({
      id,
      name: doc.meta.name,
      kind: doc.meta.kind,
      group: doc.meta.group,
      status: "available",
      source: doc.meta.source ?? `src/components/ui/${id}.tsx`,
      exports: [],
      states: [],
      keyboard: null,
      use: null,
      avoid: null,
      related: [],
    }));
  return [...catalog, ...extra];
}

export function docFor(id: string): GalleryDoc | undefined {
  return Object.hasOwn(REGISTRY, id) ? REGISTRY[id] : undefined;
}
