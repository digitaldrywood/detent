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
  MegaphoneIcon,
  PaletteIcon,
  PanelsTopLeftIcon,
  PlugIcon,
  ReceiptTextIcon,
  Settings2Icon,
  UsersIcon,
} from "lucide-react";

import type { ComponentType } from "react";
import { basePath } from "../../runtime/basePath.ts";

export interface SettingsNavItem {
  readonly id: string;
  readonly group?: "organization" | "account";
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
  "api-keys",
  "organization",
  "organization/members",
  "organization/models",
  "projects",
  "runners",
  "integrations",
  "mcp",
  "plan",
  "billing",
  "keybindings",
  "whats-new",
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
  "api-keys": "API keys",
  organization: "Organization",
  "organization/members": "Members",
  "organization/models": "Models",
  projects: "Projects",
  runners: "Runners",
  integrations: "Integrations",
  mcp: "API keys & MCP",
  plan: "Plan & usage",
  billing: "Billing",
  keybindings: "Keybindings",
  "whats-new": "What's new",
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
  selfHosted = false,
}: {
  readonly canManage: boolean;
  readonly supporting: boolean;
  readonly selfHosted?: boolean;
}): readonly SettingsNavItem[] {
  const items: SettingsNavItem[] = [
    { id: "general", label: SETTINGS_SECTION_LABELS.general, icon: Settings2Icon },
    { id: "organization", label: SETTINGS_SECTION_LABELS.organization, icon: UsersIcon },
    { id: "organization/members", label: "Members", icon: UsersIcon },
    { id: "organization/models", label: "Models", icon: BotIcon },

    { id: "projects", label: SETTINGS_SECTION_LABELS.projects, icon: PanelsTopLeftIcon },
    { id: "runners", label: SETTINGS_SECTION_LABELS.runners, icon: BotIcon },
    { id: "integrations", label: SETTINGS_SECTION_LABELS.integrations, icon: BlocksIcon },
    { id: "mcp", label: SETTINGS_SECTION_LABELS.mcp, icon: PlugIcon },
  ];
  if (canManage && !selfHosted) {
    items.push({ id: "plan", label: SETTINGS_SECTION_LABELS.plan, icon: ReceiptTextIcon });
    if (!supporting) {
      items.push({ id: "billing", label: SETTINGS_SECTION_LABELS.billing, icon: CreditCardIcon });
    }
  }
  items.push(
    { id: "appearance", label: "Appearance", icon: PaletteIcon, disabled: true, reason: WHY_DISABLED },
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
    { id: "whats-new", label: SETTINGS_SECTION_LABELS["whats-new"], icon: MegaphoneIcon },
    { id: "about", label: SETTINGS_SECTION_LABELS.about, icon: InfoIcon },
  );
  return items.filter((item) => !selfHosted || !["organization/members", "mcp"].includes(item.id)).map((item) => ({ ...item,
    label: item.id === "mcp" && !basePath().startsWith("/organizations/") ? "API & MCP" : item.label,
    group: ["general", "whats-new", "about"].includes(item.id) ? undefined : ["appearance", "keybindings", "snap-shot", "source-control", "connections", "archived"].includes(item.id) ? "account" : "organization",
  }));
}
