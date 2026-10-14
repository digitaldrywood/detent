import type { ReportsReport } from "../../contracts/reports.ts";

type Cause =
  ReportsReport["analytics"]["lane_residence"]["rework_causes"][number];
const LABELS = [
  "Conflict on landing",
  "Review transition",
  "Permission wait",
  "Lifetime limit",
  "Terminal attempt without work product",
];

function labelFor(cause: Cause): string {
  const reason = cause.reason_detail.toLowerCase().replaceAll("_", " ");
  if (cause.from_state === "Merging" && reason.includes("conflict"))
    return LABELS[0]!;
  if (
    cause.from_state === "Human Review" ||
    reason.includes("review transition")
  )
    return LABELS[1]!;
  if (/permission.*(wait|denied|required)/.test(reason)) return LABELS[2]!;
  if (/lifetime.*limit/.test(reason)) return LABELS[3]!;
  if (/terminal.*(without|no).*work product/.test(reason)) return LABELS[4]!;
  return cause.reason_detail || "Cause not recorded";
}

export function reworkByCause(
  causes: readonly Cause[],
): readonly { label: string; count: number; details: string }[] {
  const grouped = new Map<
    string,
    { label: string; count: number; details: string[] }
  >();
  for (const cause of causes) {
    const label = labelFor(cause);
    const row = grouped.get(label) ?? { label, count: 0, details: [] };
    row.count += cause.count;
    row.details.push(
      `${cause.from_state}: ${cause.reason_detail || "Not recorded"}`,
    );
    grouped.set(label, row);
  }
  return [...grouped.values()]
    .toSorted((a, b) => {
      const order = (label: string) => {
        const index = LABELS.indexOf(label);
        return index < 0 ? LABELS.length : index;
      };
      return order(a.label) - order(b.label) || a.label.localeCompare(b.label);
    })
    .map((row) => ({ ...row, details: row.details.join("; ") }));
}
