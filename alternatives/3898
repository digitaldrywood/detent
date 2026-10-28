import type { FleetRunner } from "../../contracts/account.ts";
import type {
  ActivityIssue,
  ActivityReport,
} from "../../contracts/activity.ts";
import { formatDuration } from "../../runtime/support/orchestrationTiming.ts";

export type ActivityAttempt = ActivityReport["running"][number];
export type Group = "none" | "runner" | "project" | "lane";
export type Sort = "attention" | "longest" | "started" | "number";
export const runnerName = (runner: FleetRunner) =>
  runner.display_name || runner.hostname;
export const runnerCapacity = (runner: FleetRunner) =>
  Math.min(runner.capacity_limit, runner.reported_capacity);
export const runnerCategory = (runner: FleetRunner) =>
  runner.state === "active" &&
  ["healthy", "online", "needs_attention", "asleep"].includes(runner.health)
    ? runner.leases.length > 0
      ? "working"
      : "idle"
    : "offline";

export type { ActivityIssue };
export type ActivityLane = ActivityIssue["lanes"][number];

export function displayDuration(seconds: number) {
  return formatDuration(
    seconds < 60 ? seconds * 1000 : Math.floor(seconds / 60) * 60_000,
  );
}

export function issueTiming(
  issue: ActivityIssue,
  report: ActivityReport,
  now: number,
  finished: boolean,
) {
  const elapsed =
    finished || !issue.clock_started_at
      ? 0
      : Math.max(0, (now - Date.parse(report.observed_at)) / 1000);
  const total = issue.total_seconds + elapsed;
  const typical = report.issue_typical_durations?.find(
    (row) => row.project_id === issue.project_id,
  );
  const baseline = typical && !typical.partial && typical.total.count > 0;
  const slow =
    !!issue.clock_started_at && !!baseline && total > typical.total.p90_seconds;
  const worked = Math.min(
    total,
    issue.worked_seconds + (issue.worked_running ? elapsed : 0),
  );
  const share = total > 0 ? Math.min(1, Math.max(0, worked / total)) : 0;
  const baselineText = baseline
    ? `Typical in ${issue.project_name}: ${displayDuration(typical.total.p50_seconds)} median, ${displayDuration(typical.total.p90_seconds)} for the slowest 10%.`
    : `Typical time in ${issue.project_name} is unavailable.`;
  return {
    total,
    worked,
    waiting: Math.max(0, total - worked),
    share,
    slow,
    elapsed,
    workedTone: share < 0.5 ? "warning" : "working",
    typical,
    baselineText,
  } as const;
}

export function laneTiming(
  lane: ActivityLane,
  issue: ActivityIssue,
  report: ActivityReport,
  now: number,
  finished: boolean,
) {
  const metrics = issueTiming(issue, report, now, finished);
  const seconds =
    lane.seconds + (lane.current && !finished ? metrics.elapsed : 0);
  const typical = metrics.typical?.lanes[lane.name];
  const baseline =
    metrics.typical && !metrics.typical.partial && typical && typical.count > 0;
  const slow = !!baseline && seconds > typical.p90_seconds;
  const baselineText = baseline
    ? `Typical in ${issue.project_name}: ${displayDuration(typical.p50_seconds)} median, ${displayDuration(typical.p90_seconds)} for the slowest 10%.`
    : `Typical time in ${issue.project_name} is unavailable.`;
  return {
    seconds,
    slow,
    baselineText,
    visited: lane.entries > 0,
    tone: slow ? "warning" : lane.current && !finished ? "working" : "muted",
  } as const;
}

export function issueMeta(issue: ActivityIssue, finished: boolean) {
  return {
    prefix: finished
      ? (issue.terminal_lane ?? issue.current_lane)
      : `in ${issue.clock_lane} since`,
    at: finished
      ? (issue.finished_at ?? issue.clock_started_at)
      : issue.clock_started_at,
  };
}

export function activityGroups(
  issues: readonly ActivityIssue[],
  report: ActivityReport,
  runners: readonly FleetRunner[],
  project: string,
  runner: string,
  group: Group,
  sort: Sort,
  now: number,
  finished: boolean,
) {
  const names = new Map(runners.map((row) => [row.id, runnerName(row)]));
  const rows = issues
    .filter(
      (row) =>
        (!project || row.project_id === project) &&
        (!runner || row.runner_ids.includes(runner)),
    )
    .toSorted((a, b) => {
      const aTime = issueTiming(a, report, now, finished);
      const bTime = issueTiming(b, report, now, finished);
      const byTime = bTime.total - aTime.total;
      const primary = finished
        ? Date.parse(b.finished_at ?? b.clock_started_at ?? "") -
          Date.parse(a.finished_at ?? a.clock_started_at ?? "")
        : sort === "attention"
          ? Number(
              bTime.slow ||
                b.lanes.some(
                  (lane) => laneTiming(lane, b, report, now, false).slow,
                ),
            ) -
              Number(
                aTime.slow ||
                  a.lanes.some(
                    (lane) => laneTiming(lane, a, report, now, false).slow,
                  ),
              ) || byTime
          : sort === "longest"
            ? byTime
            : sort === "started"
              ? Date.parse(b.clock_started_at ?? "") -
                Date.parse(a.clock_started_at ?? "")
              : a.number - b.number;
      return primary || a.work_item_id.localeCompare(b.work_item_id);
    });
  const groups = new Map<
    string,
    { id: string; label: string; rows: ActivityIssue[] }
  >();
  for (const row of rows) {
    const id =
      group === "runner"
        ? row.runner_id
        : group === "project"
          ? row.project_id
          : group === "lane"
            ? row.current_lane
            : "all";
    const label =
      group === "runner"
        ? (names.get(id) ?? id)
        : group === "project"
          ? row.project_name
          : group === "lane"
            ? row.current_lane
            : "";
    const entry = groups.get(id) ?? { id, label, rows: [] };
    entry.rows.push(row);
    groups.set(id, entry);
  }
  return [...groups.values()];
}
