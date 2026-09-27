import {
  ArchiveIcon,
  BlocksIcon,
  BotIcon,
  createLucideIcon,
  CreditCardIcon,
  GitBranchIcon,
  InfoIcon,
  KeyboardIcon,
  Link2Icon,
  PaletteIcon,
  PanelsTopLeftIcon,
  ReceiptTextIcon,
  Settings2Icon,
  UsersIcon,
} from "lucide-react";

import type { ComponentType } from "react";

export interface SettingsNavItem {
  readonly id: string;
  readonly label: string;
  readonly icon: ComponentType<{ className?: string }>;
  /** A route to navigate to, or nothing for `/settings/<id>`. */
  readonly to?: string;

  readonly disabled?: boolean;
  /** What the tooltip says about a disabled row. */
  readonly reason?: string;
}

const SnapShotIcon = createLucideIcon("snap-shot", [
  [
    "path",
    {
      d: "M8 3H6a3 3 0 0 0-3 3v2M16 3h2a3 3 0 0 1 3 3v2M21 16v2a3 3 0 0 1-3 3h-2M8 21H6a3 3 0 0 1-3-3v-2",
      key: "capture-frame",
    },
  ],
  ["rect", { width: "10", height: "8", x: "7", y: "8", rx: "2", key: "window" }],
  ["circle", { cx: "12", cy: "12", r: "1.5", key: "lens" }],
]);

/** The sections this client serves, by path segment. */
export const SETTINGS_SECTION_IDS = [
  "general",
  "organization",
  "projects",
  "runners",
  "integrations",
  "plan",
  "billing",
  "keybindings",
  "about",
] as const;
export type SettingsSectionId = (typeof SETTINGS_SECTION_IDS)[number];

export const DEFAULT_SECTION: SettingsSectionId = "general";

export function isSettingsSectionId(value: string): value is SettingsSectionId {
  return (SETTINGS_SECTION_IDS as readonly string[]).includes(value);
}

/** What the breadcrumb's second item says, per section. */
export const SETTINGS_SECTION_LABELS: Readonly<Record<SettingsSectionId, string>> = {
  general: "General",
  organization: "Organization",
  projects: "Projects",
  runners: "Providers & runners",
  integrations: "Integrations",
  plan: "Plan",
  billing: "Billing",
  keybindings: "Keybindings",
  about: "About",
};

const WHY_DISABLED =
  "This settings section is not available in Detent Cloud yet.";

/**
 * The navigation, for one actor. Plan and billing are owner or admin only and
 * are not rendered at all for anybody else: the hub refuses them with `403`,
 * and a section that can only ever say "you cannot see this" is noise on a
 * settings page.
 */
export function settingsNavItems({
  canManage,
  supporting,
}: {
  readonly canManage: boolean;
  readonly supporting: boolean;
}): readonly SettingsNavItem[] {
  const items: SettingsNavItem[] = [
    { id: "general", label: SETTINGS_SECTION_LABELS.general, icon: Settings2Icon },
    { id: "organization", label: SETTINGS_SECTION_LABELS.organization, icon: UsersIcon },

    { id: "appearance", label: "Appearance", icon: PaletteIcon, disabled: true, reason: WHY_DISABLED },
    { id: "projects", label: SETTINGS_SECTION_LABELS.projects, icon: PanelsTopLeftIcon },
    { id: "runners", label: SETTINGS_SECTION_LABELS.runners, icon: BotIcon },
    { id: "integrations", label: SETTINGS_SECTION_LABELS.integrations, icon: BlocksIcon },
  ];
  if (canManage) {
    items.push({ id: "plan", label: SETTINGS_SECTION_LABELS.plan, icon: ReceiptTextIcon });
    if (!supporting) {
      items.push({ id: "billing", label: SETTINGS_SECTION_LABELS.billing, icon: CreditCardIcon });
    }
  }
  items.push(
    { id: "keybindings", label: SETTINGS_SECTION_LABELS.keybindings, icon: KeyboardIcon },
    { id: "snap-shot", label: "SnapShots", icon: SnapShotIcon, disabled: true, reason: WHY_DISABLED },
    {
      id: "source-control",
      label: "Source Control",
      icon: GitBranchIcon,
      disabled: true,
      reason: WHY_DISABLED,
    },
    {
      id: "connections",
      label: "Connections",
      icon: Link2Icon,
      disabled: true,
      reason: WHY_DISABLED,
    },
    { id: "archived", label: "Archive", icon: ArchiveIcon, disabled: true, reason: WHY_DISABLED },
    { id: "about", label: SETTINGS_SECTION_LABELS.about, icon: InfoIcon },
  );
  return items;
}
