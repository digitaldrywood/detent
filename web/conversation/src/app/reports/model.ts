import type { MetricDelta } from "../../components/ui/metric-tile.tsx";
import type { ReportsRange, ReportsReport } from "../../contracts/reports.ts";
import { formatDuration } from "../adapters/usageLimits.ts";
import { DEFAULT_REPORTS_RANGE } from "./reportsQuery.ts";
import { formatCount, formatDayShort, formatHourShort, formatUsd } from "../usage/usageFormat.ts";

export const REPORT_TABS = [
  { id: "overview", label: "Overview" },
  { id: "flow", label: "Flow" },
  { id: "quality", label: "Quality" },
  { id: "cost", label: "Cost" },
  { id: "failures", label: "Failures" },
] as const;
export type ReportTab = (typeof REPORT_TABS)[number]["id"];

export const REPORT_RANGES: readonly { value: ReportsRange; label: string; long: string; prior: string }[] = [
  { value: "24h", label: "24h", long: "Last 24 hours", prior: "prior 24h" },
  { value: "7d", label: "7d", long: "Last 7 days", prior: "prior 7d" },
  { value: "30d", label: "30d", long: "Last 30 days", prior: "prior 30d" },
  { value: "90d", label: "90d", long: "Last 90 days", prior: "prior 90d" },
];
export const DEFAULT_RANGE: ReportsRange = DEFAULT_REPORTS_RANGE;

export interface ReportsSearch {
  readonly range: ReportsRange;
  readonly compare: boolean;
  readonly clock: "todo" | "created";
  readonly group: "stage" | "model" | "runner";
  readonly issues: "all" | "shipped" | "not-shipped";
  readonly kind: "all" | "infrastructure" | "issue";
}

export function isReportTab(value: unknown): value is ReportTab {
  return REPORT_TABS.some((tab) => tab.id === value);
}

function pick<T extends string>(value: unknown, allowed: readonly T[], fallback: T): T {
  return allowed.includes(value as T) ? (value as T) : fallback;
}

export function parseReportsSearch(search: Readonly<Record<string, unknown>>): ReportsSearch {
  return {
    range: pick(search.range, REPORT_RANGES.map((range) => range.value), DEFAULT_RANGE),
    compare: search.compare !== "off",
    clock: pick(search.clock, ["todo", "created"], "todo"),
    group: pick(search.group, ["stage", "model", "runner"], "stage"),
    issues: pick(search.issues, ["all", "shipped", "not-shipped"], "all"),
    kind: pick(search.kind, ["all", "infrastructure", "issue"], "all"),
  };
}

/** The query string for a view: defaults are left out so a plain link stays plain. */
export function reportsSearchParams(search: ReportsSearch): Record<string, string> {
  const out: Record<string, string> = {};
  if (search.range !== DEFAULT_RANGE) out.range = search.range;
  if (!search.compare) out.compare = "off";
  if (search.clock !== "todo") out.clock = search.clock;
  if (search.group !== "stage") out.group = search.group;
  if (search.issues !== "all") out.issues = search.issues;
  if (search.kind !== "all") out.kind = search.kind;
  return out;
}

export function rangeOption(range: ReportsRange) {
  return REPORT_RANGES.find((option) => option.value === range) ?? REPORT_RANGES[1]!;
}

export const duration = (seconds: number) => formatDuration(seconds * 1000);

export const hoursLabel = (hours: number) => formatDuration(hours * 3_600_000);

/** A count delta: `▲ 38 vs prior 7d`. */
export function countDelta(current: number, previous: number | undefined, higherIsBetter: boolean, suffix = ""): MetricDelta {
  if (previous === undefined) return null;
  const change = current - previous;
  return {
    text: `${formatCount(Math.abs(change))}${suffix}`,
    direction: change > 0 ? "up" : change < 0 ? "down" : "flat",
    tone: change === 0 ? "neutral" : change > 0 === higherIsBetter ? "good" : "bad",
  };
}

export function durationDelta(current: number, previous: number | undefined): MetricDelta {
  if (previous === undefined || previous <= 0 || current <= 0) return null;
  const change = current - previous;
  return {
    text: change === 0 ? "no change" : `${duration(Math.abs(change))} ${change < 0 ? "faster" : "slower"}`,
    direction: change > 0 ? "up" : change < 0 ? "down" : "flat",
    tone: change === 0 ? "neutral" : change < 0 ? "good" : "bad",
  };
}

export function pointsDelta(current: number | null, previous: number | null | undefined, higherIsBetter = true): MetricDelta {
  if (current === null || previous === null || previous === undefined) return null;
  const change = Math.round(current - previous);
  return {
    text: `${Math.abs(change)} pts`,
    direction: change > 0 ? "up" : change < 0 ? "down" : "flat",
    tone: change === 0 ? "neutral" : change > 0 === higherIsBetter ? "good" : "bad",
  };
}

export function usdDelta(current: number | null, previous: number | null | undefined): MetricDelta {
  if (current === null || previous === null || previous === undefined) return null;
  const change = current - previous;
  return {
    text: formatUsd(Math.abs(change)),
    direction: change > 0 ? "up" : change < 0 ? "down" : "flat",
    tone: Math.abs(change) < 0.005 ? "neutral" : change < 0 ? "good" : "bad",
  };
}

export const percentText = (value: number) => `${Math.round(value)}%`;

export type SeriesBucket = NonNullable<ReportsReport["series"]>[number];

const DAY = 86_400_000;
const DAILY_SPAN = 6 * DAY;

/** The report's buckets at display grain: the Hub's series, or its throughput rolled up to UTC calendar days. */
export function reportBuckets(report: ReportsReport): readonly SeriesBucket[] {
  if (report.series !== undefined) return report.series;
  const from = Date.parse(report.analytics.window.from);
  const to = Date.parse(report.analytics.window.to);
  const daily = to - from >= DAILY_SPAN;
  const origin = daily ? Math.floor(from / DAY) * DAY : from;
  const step = daily ? DAY : 3_600_000;
  const empty = { count: 0, seconds: 0, p50_seconds: 0, p90_seconds: 0 };
  const buckets = new Map<number, SeriesBucket>();
  for (const bucket of report.throughput) {
    const index = Math.floor((Date.parse(bucket.from) - origin) / step);
    const start = origin + index * step;
    const current = buckets.get(index) ?? {
      from: new Date(Math.max(start, from)).toISOString(),
      to: new Date(Math.min(start + step, to)).toISOString(),
      done: 0,
      failed: 0,
      failed_infrastructure: 0,
      failed_issue: 0,
      cycle: empty,
      lead: empty,
      lane_seconds: {},
      stage_cost: {},
      not_shipped_cost: 0,
      rework_classes: {},
    };
    buckets.set(index, { ...current, done: current.done + bucket.done, failed: current.failed + bucket.failed });
  }
  return [...buckets.entries()].toSorted(([a], [b]) => a - b).map(([, bucket]) => bucket);
}

/** Axis labels from each bucket's UTC calendar day, or its UTC hour for a short window. */
export function bucketCategories(buckets: readonly SeriesBucket[]) {
  const first = buckets[0];
  const last = buckets.at(-1);
  const daily = first !== undefined && last !== undefined && Date.parse(last.to) - Date.parse(first.from) >= DAILY_SPAN;
  return buckets.map((bucket) => {
    const day = bucket.from.slice(0, 10);
    return daily
      ? { key: bucket.from, label: formatDayShort(day), shortLabel: String(Number(day.slice(8, 10))) }
      : { key: bucket.from, label: formatHourShort(bucket.from, "UTC"), shortLabel: formatHourShort(bucket.from, "UTC") };
  });
}

/** Lane colours: queue in info, active work in success, returns in warning, held work muted. */
export const LANE_COLORS: Readonly<Record<string, string>> = {
  Todo: "var(--info)",
  "In Progress": "var(--success)",
  Rework: "var(--warning)",
  Merging: "color-mix(in oklab, var(--contrast-foreground) 72%, var(--background))",
  Held: "var(--muted-foreground)",
};
export const laneColor = (lane: string) => LANE_COLORS[lane] ?? LANE_COLORS.Held!;
export const HELD_LANES = new Set(["Blocked", "Human Review"]);

/** Infrastructure, issue or unknown, from a failure signature class or a class total's kind. */
export function failureKind(value: string): "infrastructure" | "issue" | "unknown" {
  if (value === "infrastructure") return "infrastructure";
  if (value === "issue" || value === "source" || value === "merge" || value === "review") return "issue";
  return "unknown";
}

export const KIND_COLORS = {
  infrastructure: "var(--info)",
  issue: "var(--warning)",
  unknown: "color-mix(in oklab, var(--contrast-foreground) 30%, var(--background))",
} as const;

export const STAGE_GROUPS = [
  { id: "code", label: "Code", stages: ["code"], color: "var(--contrast-foreground)" },
  { id: "rework", label: "Rework", stages: ["rework"], color: "color-mix(in oklab, var(--contrast-foreground) 72%, var(--background))" },
  { id: "merge", label: "Merge", stages: ["merge"], color: "color-mix(in oklab, var(--contrast-foreground) 60%, var(--background))" },
  { id: "plan", label: "Plan + validate", stages: ["plan", "validator"], color: "color-mix(in oklab, var(--contrast-foreground) 44%, var(--background))" },
] as const;
export const OTHER_STAGE_COLOR = "color-mix(in oklab, var(--contrast-foreground) 30%, var(--background))";
export const NOT_SHIPPED_COLOR = "var(--warning)";

const STAGE_LABELS: Readonly<Record<string, string>> = {
  code: "Code",
  rework: "Rework",
  merge: "Merge",
  plan: "Plan",
  validator: "Validate",
  review: "Review",
};
export const stageLabel = (stage: string) => STAGE_LABELS[stage] ?? stage;

export function issueTitle(report: ReportsReport, id: string): string {
  return report.titles[id] ?? report.spend.find((item) => item.work_item_id === id)?.title ?? id;
}

/** One RFC 4180 field; a leading formula character is neutralised so a spreadsheet shows it as text. */
export function csvCell(value: string | number): string {
  const raw = String(value);
  const text = /^[=+\-@\t\r]/.test(raw) ? `'${raw}` : raw;
  return /[",\r\n]/.test(text) ? `"${text.replaceAll('"', '""')}"` : text;
}
