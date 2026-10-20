import type { FleetRunner } from "../../contracts/account.ts";
import type { ActivityReport } from "../../contracts/activity.ts";
import { formatDuration } from "../../runtime/support/orchestrationTiming.ts";

export type ActivityAttempt = ActivityReport["running"][number];
export const STAGES = ["Plan", "Code", "Review", "Merge"] as const;
export type Group = "none" | "runner" | "project" | "stage";
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

export function stageInfo(attempt: ActivityAttempt) {
  const stage = attempt.stage?.toLowerCase();
  const index =
    stage === "plan"
      ? 0
      : stage === "code" || stage === "rework"
        ? 1
        : stage === "review" || stage === "validate"
          ? 2
          : stage === "merge"
            ? 3
            : -1;
  return {
    index,
    key: stage === "rework" ? "code" : stage === "validate" ? "review" : stage,
    label:
      stage === "rework"
        ? "Rework"
        : index < 0
          ? "Stage unavailable"
          : (STAGES.at(index) ?? "Stage unavailable"),
  };
}

function displayDuration(seconds: number) {
  return formatDuration(
    seconds < 60 ? seconds * 1000 : Math.floor(seconds / 60) * 60_000,
  );
}

export function timing(
  attempt: ActivityAttempt,
  report: ActivityReport,
  now: number,
  finished: boolean,
) {
  const stage = stageInfo(attempt);
  const typical = report.typical_durations.find(
    (row) => row.project_id === attempt.project_id && row.stage === stage.key,
  );
  const baselineAvailable = typical !== undefined && !typical.partial;
  const seconds = finished
    ? attempt.stage_duration_seconds
    : attempt.stage_started_at
      ? Math.max(0, (now - Date.parse(attempt.stage_started_at)) / 1000)
      : attempt.stage_elapsed_seconds === undefined
        ? undefined
        : attempt.stage_elapsed_seconds +
          Math.max(0, (now - Date.parse(report.observed_at)) / 1000);
  const slow =
    seconds !== undefined &&
    baselineAvailable &&
    seconds > typical.p90_seconds;
  const fast =
    finished &&
    seconds !== undefined &&
    baselineAvailable &&
    seconds <= typical.p50_seconds / 2;
  const duration =
    seconds === undefined ? "Time unavailable" : displayDuration(seconds);
  const cancelled =
    attempt.outcome === "cancelled" || attempt.outcome === "interrupted";
  const progress =
    seconds === undefined
      ? 0
      : finished && !cancelled
        ? 1
        : baselineAvailable && typical.p90_seconds > 0
          ? Math.min(cancelled ? 0.75 : 1, seconds / typical.p90_seconds)
          : cancelled
            ? 0.5
            : 0;
  const tone =
    finished && attempt.outcome === "failed"
      ? "error"
      : slow
        ? "warning"
        : finished && attempt.outcome === "succeeded"
          ? "success"
          : finished
            ? "muted"
            : "working";
  const baseline = typical?.partial
    ? `Typical time in ${attempt.project_name} is unavailable because the baseline is incomplete.`
    : typical
      ? `Typical in ${attempt.project_name}: ${displayDuration(typical.p50_seconds)} median, ${displayDuration(typical.p90_seconds)} for the slowest 10%.`
      : `Typical time in ${attempt.project_name} is unavailable.`;
  const description = `${stage.label} for ${duration}. ${baseline}`;
  const label = `${stage.index < 0 ? "Stage unavailable" : `Stage ${stage.index + 1} of 4, ${stage.label}`}, ${duration}${attempt.outcome ? `, ${attempt.outcome}` : ""}${slow ? ", slow" : fast ? ", fast" : ""}. ${baseline}`;
  return {
    stageLabel: stage.label,
    ...stage,
    seconds,
    typical,
    slow,
    fast,
    duration,
    progress,
    tone,
    description,
    label,
  } as const;
}

export function activityGroups(
  attempts: readonly ActivityAttempt[],
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
  const rows = attempts
    .filter(
      (row) =>
        (!project || row.project_id === project) &&
        (!runner || row.runner_id === runner),
    )
    .toSorted((a, b) => {
      const aTiming = timing(a, report, now, finished);
      const bTiming = timing(b, report, now, finished);
      const byTime = (bTiming.seconds ?? -1) - (aTiming.seconds ?? -1);
      const primary = finished
        ? Date.parse(b.finished_at ?? b.started_at) -
          Date.parse(a.finished_at ?? a.started_at)
        : sort === "attention"
          ? Number(bTiming.slow) - Number(aTiming.slow) || byTime
          : sort === "longest"
            ? byTime
            : sort === "started"
              ? Date.parse(b.started_at) - Date.parse(a.started_at)
              : a.number - b.number;
      return primary || a.attempt_id.localeCompare(b.attempt_id);
    });
  const groups = new Map<
    string,
    { id: string; label: string; rows: ActivityAttempt[] }
  >();
  for (const row of rows) {
    const id =
      group === "runner"
        ? row.runner_id
        : group === "project"
          ? row.project_id
          : group === "stage"
            ? (stageInfo(row).key ?? "unknown")
            : "all";
    const label =
      group === "runner"
        ? (names.get(id) ?? id)
        : group === "project"
          ? row.project_name
          : group === "stage"
            ? (STAGES.find((stage) => stage.toLowerCase() === id) ??
              "Stage unavailable")
            : "";
    const entry = groups.get(id) ?? { id, label, rows: [] };
    entry.rows.push(row);
    groups.set(id, entry);
  }
  const result = [...groups.values()];
  if (group !== "stage") return result;
  const rank = (id: string) => {
    const index = STAGES.findIndex((stage) => stage.toLowerCase() === id);
    return index < 0 ? STAGES.length : index;
  };
  return result.toSorted((a, b) => rank(a.id) - rank(b.id));
}
