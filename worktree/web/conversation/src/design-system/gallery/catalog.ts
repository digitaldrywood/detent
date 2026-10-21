// The component catalog, as the gallery reads it.
//
// `src/design-system/catalog.json` is the source of truth (schema 1:
// `{ schema, entries: [...] }`). The gallery is a viewer of it, never a second
// list: an entry appears here because the catalog names it, and the registry
// only says how to render it.

import catalogFile from "../catalog.json";

export type CatalogKind = "foundation" | "primitive" | "composition" | "surface";
export type CatalogStatus = "available" | "proposed" | "exception";

export interface CatalogEntry {
  readonly id: string;
  readonly name: string;
  readonly kind: CatalogKind;
  readonly group: string;
  readonly status: CatalogStatus;
  readonly source: string | null;
  readonly exports: readonly string[];
  readonly states: readonly string[];
  readonly keyboard: string | null;
  readonly use: string | null;
  readonly avoid: string | null;
  readonly related: readonly string[];
}

/** The catalog file's contents. */
export function rawCatalog(): unknown {
  return catalogFile;
}

function text(value: unknown): string | null {
  if (typeof value === "string" && value.trim() !== "") return value;
  if (Array.isArray(value)) {
    const lines = value.filter((line): line is string => typeof line === "string");
    return lines.length > 0 ? lines.join(" ") : null;
  }
  return null;
}

function list(value: unknown): string[] {
  if (Array.isArray(value)) return value.filter((item): item is string => typeof item === "string");
  if (typeof value === "string" && value !== "") return [value];
  return [];
}

function oneOf<T extends string>(value: unknown, allowed: readonly T[], fallback: T): T {
  return allowed.includes(value as T) ? (value as T) : fallback;
}

/** "alert-dialog" → "Alert dialog". */
export function displayName(id: string): string {
  const words = id.replace(/[-_]+/g, " ").trim();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/** Reads schema 1 (`entries` with `id`, `status`, `source`) into gallery entries. */
export function normalizeCatalog(raw: unknown): CatalogEntry[] {
  if (raw === null || typeof raw !== "object") return [];
  const record = raw as Record<string, unknown>;
  const rows = Array.isArray(record.entries) ? record.entries : [];
  const entries: CatalogEntry[] = [];
  for (const row of rows) {
    if (row === null || typeof row !== "object") continue;
    const item = row as Record<string, unknown>;
    const id = text(item.id);
    if (id === null) continue;
    const name = text(item.name) ?? displayName(id);
    const source = text(item.source);
    entries.push({
      id,
      name,
      kind: oneOf(item.kind, ["primitive", "composition", "surface"] as const, "primitive"),
      group: text(item.group) ?? "other",
      status: oneOf(
        item.status,
        ["available", "proposed", "exception"] as const,
        "proposed",
      ),
      source: source === null ? null : source.replace(/^web\/conversation\//, ""),
      exports: list(item.exports),
      states: list(item.states),
      keyboard: text(item.keyboard),
      use: text(item.use),
      avoid: text(item.avoid),
      related: list(item.related),
    });
  }
  return entries;
}

const KIND_ORDER: readonly CatalogKind[] = ["foundation", "primitive", "composition", "surface"];

/** Other names people search for, by catalog id. */
export const SEARCH_ALIASES: Readonly<Record<string, readonly string[]>> = {
  dialog: ["modal"],
  "alert-dialog": ["confirm", "confirmation", "modal"],
  sheet: ["drawer", "side panel"],
  popover: ["popup", "flyout"],
  tooltip: ["hint", "title"],
  toast: ["notification", "snackbar"],
  select: ["dropdown", "picker"],
  menu: ["dropdown", "context menu", "actions"],
  combobox: ["typeahead", "multi select"],
  autocomplete: ["typeahead", "suggestions"],
  switch: ["toggle", "on off"],
  checkbox: ["tick"],
  badge: ["tag", "chip", "pill", "label"],
  separator: ["divider", "rule"],
  skeleton: ["placeholder", "loading"],
  spinner: ["loader", "loading", "progress"],
  empty: ["empty state", "zero state", "blank"],
  kbd: ["shortcut", "keyboard", "hotkey"],
  "scroll-area": ["scrollbar"],
  collapsible: ["accordion", "disclosure", "expand"],
  "toggle-group": ["segmented control", "tabs"],
  "right-panel-tabs": ["tabs"],
  "thread-status-indicators": ["status dot", "indicator"],
  "brand-icons": ["logo", "icon"],
};

/** Case- and accent-insensitive match on the entry's name, id, group, source, exports, contract text and aliases. */
export function matchesQuery(entry: CatalogEntry, query: string): boolean {
  const needle = fold(query);
  if (needle === "") return true;
  const haystack = fold(
    [
      entry.id,
      entry.name,
      entry.group,
      entry.kind,
      entry.source ?? "",
      ...entry.exports,
      entry.use ?? "",
      entry.avoid ?? "",
      entry.keyboard ?? "",
      ...entry.states,
      ...(SEARCH_ALIASES[entry.id] ?? []),
    ].join(" "),
  );
  return needle.split(" ").every((word) => haystack.includes(word));
}

function fold(value: string): string {
  return value.normalize("NFKD").replace(/\p{M}/gu, "").toLowerCase().replace(/\s+/g, " ").trim();
}

export interface NavGroup {
  readonly key: string;
  readonly kind: CatalogKind;
  readonly group: string;
  readonly entries: readonly CatalogEntry[];
}

/** Foundations first, then primitives, compositions and surfaces; groups and names alphabetical. */
export function groupEntries(entries: readonly CatalogEntry[], query = ""): NavGroup[] {
  const groups = new Map<string, NavGroup & { entries: CatalogEntry[] }>();
  for (const entry of entries) {
    if (!matchesQuery(entry, query)) continue;
    const key = `${entry.kind}/${entry.group}`;
    let group = groups.get(key);
    if (group === undefined) {
      group = { key, kind: entry.kind, group: entry.group, entries: [] };
      groups.set(key, group);
    }
    group.entries.push(entry);
  }
  return [...groups.values()]
    .sort(
      (a, b) =>
        KIND_ORDER.indexOf(a.kind) - KIND_ORDER.indexOf(b.kind) || a.group.localeCompare(b.group),
    )
    .map((group) => ({
      ...group,
      entries: [...group.entries].sort((a, b) => a.name.localeCompare(b.name)),
    }));
}
