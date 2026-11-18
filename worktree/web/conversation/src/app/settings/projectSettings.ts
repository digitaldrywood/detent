export const PROJECT_SETTINGS_TABS = [
  { id: "general", label: "General" },
  { id: "repository", label: "Repository & GitHub" },
  { id: "workflow", label: "Workflow" },
  { id: "models", label: "Models" },
  { id: "runners", label: "Runners & Sprites" },
  { id: "danger", label: "Danger zone" },
] as const;

export type ProjectSettingsTab = (typeof PROJECT_SETTINGS_TABS)[number]["id"];

export function projectSettingsTab(value?: string): ProjectSettingsTab {
  return PROJECT_SETTINGS_TABS.find((tab) => tab.id === value)?.id ?? "general";
}

export function projectSettingsPath(project: { readonly id: string; readonly slug?: string } | string, tab: ProjectSettingsTab = "general"): `/settings/${string}` {
  const identifier = typeof project === "string" ? project : project.slug || project.id;
  return `/settings/projects/${encodeURIComponent(identifier)}/${tab}`;
}
