import { PROJECT_SETTINGS_TABS, projectSettingsPath } from "../../app/settings/projectSettings.ts";
import {
  SETTINGS_SECTION_IDS,
  SETTINGS_SECTION_LABELS as SECTION_LABELS_BY_ID,
  type SettingsSectionId,
} from "../../app/settings/sections.tsx";

export type SettingsPath = `/settings/${string}`;

export interface SettingsSearchItem {
  readonly id: string;
  readonly title: string;
  readonly to: SettingsPath;
  /** The element a hit scrolls to, when the section publishes one. */
  readonly targetId?: string;
  /** Extra words a reader might search for instead of the title. */
  readonly keywords?: string;
}

export function settingsSectionPath(id: SettingsSectionId): SettingsPath {
  return `/settings/${id}`;
}

export const SETTINGS_SECTION_LABELS: Readonly<Record<string, string>> = Object.fromEntries(
  SETTINGS_SECTION_IDS.map((id) => [settingsSectionPath(id), SECTION_LABELS_BY_ID[id]]),
);

interface IndexEntry {
  readonly section: SettingsSectionId;
  readonly title: string;
  readonly targetId?: string;
  readonly keywords?: string;
}

const SETTINGS_INDEX: readonly IndexEntry[] = [
  { section: "organization", title: "Name", keywords: "rename organization" },
  { section: "organization", title: "Your organizations", keywords: "create switch organization workspace" },
  { section: "organization/models", targetId: "settings-organization-models", title: "Model selection", keywords: "models effort planning implementation validation defaults" },
  {
    section: "general",
    title: "Organization",
    targetId: "settings-general-organization",
    keywords: "switch scope account",
  },
  {
    section: "general",
    title: "Your account",
    targetId: "settings-general-account",
    keywords: "sign out session email",
  },
  {
    section: "general",
    title: "Plan",
    targetId: "settings-general-plan",
    keywords: "entitlement allowance tier",
  },
  { section: "organization/members", targetId: "settings-organization-members", title: "Members", keywords: "people roles owner admin viewer access" },
  { section: "organization/members", targetId: "settings-organization-invitations", title: "Invite member", keywords: "invitations invitation email join" },
  {
    section: "projects",
    title: "Projects",
    targetId: "settings-projects",
    keywords: "board workflow new project write access",
  },
  {
    section: "runners",
    title: "Providers",
    targetId: "settings-providers",
    keywords: "claude codex model capacity",
  },
  {
    section: "runners",
    title: "Runners",
    targetId: "settings-runners",
    keywords: "fleet agent capacity busy host",
  },
  {
    section: "plan",
    title: "Allowances",
    targetId: "settings-plan",
    keywords: "limit usage entitlement quota over",
  },
  {
    section: "billing",
    title: "Subscription",
    targetId: "settings-billing",
    keywords: "invoice card payment seat portal",
  },
  {
    section: "keybindings",
    title: "New conversation",
    targetId: "settings-keybindings",
    keywords: "shortcut keyboard compose",
  },
  {
    section: "keybindings",
    title: "Focus search",
    targetId: "settings-keybindings",
    keywords: "shortcut keyboard slash",
  },
  { section: "whats-new", title: "What's new", targetId: "settings-whats-new", keywords: "release notes changelog new improved fixed builds" },
  { section: "integrations", title: "Slack", targetId: "slack-notifications", keywords: "notifications incoming webhook" },
  { section: "integrations", title: "Sprites", targetId: "sprites", keywords: "organization token Fly" },
  { section: "about", title: "Detent", targetId: "settings-about", keywords: "version build hub api" },
  {
    section: "about",
    title: "Interface",
    targetId: "settings-about",
    keywords: "licence license t3 code mit",
  },
];

/** The index, restricted to the sections this actor can actually reach. */
export function settingsSearchItems(
  reachable: readonly SettingsSectionId[],
): readonly SettingsSearchItem[] {
  const allowed = new Set<string>(reachable);
  return SETTINGS_INDEX.filter((entry) => allowed.has(entry.section)).map((entry) => ({
    // Its own id, never the anchor's: several rows of one section share an
    // anchor, and the id is what the listbox keys and points
    // `aria-activedescendant` at.
    id: `${entry.section}-${entry.title.toLowerCase().replaceAll(/[^a-z0-9]+/g, "-")}`,
    title: entry.title,
    to: settingsSectionPath(entry.section),
    ...(entry.targetId === undefined ? {} : { targetId: entry.targetId }),
    ...(entry.keywords === undefined ? {} : { keywords: entry.keywords }),
  }));
}

export function projectSettingsSearchItems(projects: readonly { readonly id: string; readonly name: string; readonly slug?: string }[]): readonly SettingsSearchItem[] {
  return projects.flatMap((project) => PROJECT_SETTINGS_TABS.map((tab) => ({
    id: `project-${project.id}-${tab.id}`,
    title: `${project.name} · ${tab.label}`,
    to: projectSettingsPath(project, tab.id),
    keywords: `project settings ${tab.id === "general" ? "identity slug archive duplicate" : tab.id === "repository" ? "github pull request branch remote integration" : tab.id === "workflow" ? "policy states lanes" : tab.id === "models" ? "selection effort planning implementation validation" : tab.id === "runners" ? "routing dispatch provider sprite token" : "delete project"}`,
  })));
}

export function searchSettings(
  query: string,
  items: readonly SettingsSearchItem[],
): readonly SettingsSearchItem[] {
  const needle = query.trim().toLowerCase();
  if (needle === "") return [];
  const scored: { readonly item: SettingsSearchItem; readonly score: number }[] = [];
  for (const item of items) {
    const title = item.title.toLowerCase();
    const section = (SETTINGS_SECTION_LABELS[item.to] ?? "").toLowerCase();
    const keywords = (item.keywords ?? "").toLowerCase();
    const score = title.startsWith(needle)
      ? 0
      : title.includes(needle)
        ? 1
        : section.includes(needle)
          ? 2
          : keywords.includes(needle)
            ? 3
            : -1;
    if (score >= 0) scored.push({ item, score });
  }
  return scored
    .map((entry, index) => ({ ...entry, index }))
    .toSorted((a, b) => a.score - b.score || a.index - b.index)
    .map((entry) => entry.item);
}
