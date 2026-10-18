/** Chart groups for the Hub's rework classes: issue causes in amber shades, infrastructure in blue. */
export const REWORK_GROUPS = [
  { id: "review", label: "Review findings", color: "var(--warning)" },
  { id: "gate", label: "Gate failed", color: "color-mix(in oklab, var(--warning) 70%, var(--background))" },
  { id: "conflict", label: "Conflict on landing", color: "color-mix(in oklab, var(--warning) 42%, var(--background))" },
  { id: "refused", label: "Landing refused", color: "var(--info)" },
  { id: "other", label: "Other", color: "var(--muted-foreground)" },
] as const;
export type ReworkGroup = (typeof REWORK_GROUPS)[number]["id"];

const CLASS_GROUPS: Readonly<Record<string, ReworkGroup>> = {
  "Review findings to address": "review",
  "Gate failed before landing": "gate",
  "Conflict on landing": "conflict",
  "Landing refused: protected base": "refused",
  "Base moved after validation": "refused",
};

/** The chart group a rework class belongs to; an unnamed infrastructure class joins the blue group. */
export function reworkGroupOf(label: string, kind: string): (typeof REWORK_GROUPS)[number] {
  const id = CLASS_GROUPS[label] ?? (kind === "infrastructure" ? "refused" : "other");
  return REWORK_GROUPS.find((group) => group.id === id)!;
}

/** Folds one bucket's class counts into the chart groups. */
export function reworkByGroup(classes: Readonly<Record<string, number>>, kinds: Readonly<Record<string, string>>): Readonly<Record<ReworkGroup, number>> {
  const out: Record<ReworkGroup, number> = { review: 0, gate: 0, conflict: 0, refused: 0, other: 0 };
  for (const [label, count] of Object.entries(classes)) out[reworkGroupOf(label, kinds[label] ?? "unknown").id] += count;
  return out;
}
