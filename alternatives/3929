import type { CollaborationEvent, NativeAttempt, NativeIssue } from "../../../contracts/work.ts";
import type { IssueExplanation } from "../../../contracts/diagnostics.ts";

export function attemptStage(attempt: NativeAttempt): string {
  return attempt.runtime?.activity?.stage || attempt.identity?.role || attempt.runtime?.phase || "unavailable";
}

export function failureText(attempt: NativeAttempt): string {
  return (attempt.terminal_failure?.summary || attempt.outcome || "").trim().replace(/\s+/g, " ");
}

export function attemptIdentityLabel(attempt: NativeAttempt): string {
  const identity = attempt.runtime?.identity;
  const model = identity?.resolved_model?.value || identity?.requested_model?.value || attempt.identity?.model;
  if (!model) return /merg/i.test(attemptStage(attempt)) ? "identity not recorded" : "unavailable";
  return `${model} · ${identity?.reasoning_effort?.value || "effort unavailable"}`;
}

export function groupAttempts(attempts: readonly NativeAttempt[]): readonly (readonly NativeAttempt[])[] {
  const ordered = [...attempts].sort((a, b) => Date.parse(a.started_at) - Date.parse(b.started_at) || a.attempt_id.localeCompare(b.attempt_id));
  const groups: NativeAttempt[][] = [];
  for (const attempt of ordered) {
    const previous = groups.at(-1)?.at(-1);
    if (previous !== undefined && attempt.status === "failed" && previous.status === "failed"
      && failureText(attempt) !== "" && failureText(attempt) === failureText(previous)
      && attemptStage(attempt) === attemptStage(previous)) {
      groups.at(-1)!.push(attempt);
    } else {
      groups.push([attempt]);
    }
  }
  return groups;
}

export function laneClock(history: readonly CollaborationEvent[], attempts: readonly NativeAttempt[], now: number, terminal: boolean) {
  const transitions = history.filter((event) => event.type === "workflow.transitioned" && event.data.to_state !== undefined && Number.isFinite(Date.parse(event.recorded_at)))
    .sort((a, b) => Date.parse(a.recorded_at) - Date.parse(b.recorded_at) || (BigInt(a.aggregate_sequence) < BigInt(b.aggregate_sequence) ? -1 : 1));
  const first = transitions.findIndex((event) => event.data.to_state?.toLowerCase() === "todo");
  if (first === -1) return null;
  const selected = transitions.slice(first);
  const start = Date.parse(selected[0]!.recorded_at);
  const end = terminal ? Date.parse(selected.at(-1)!.recorded_at) : now;
  const lanes = new Map<string, number>();
  const laneIntervals: (readonly [number, number])[] = [];
  for (let i = 0; i < selected.length; i++) {
    const transition = selected[i]!;
    const lane = transition.data.to_state!;
    if (lane.toLowerCase() === "backlog") continue;
    const duration = Math.max(0, Math.min(end, Date.parse(selected[i + 1]?.recorded_at ?? new Date(end).toISOString())) - Date.parse(transition.recorded_at));
    if (duration > 0) {
      lanes.set(lane, (lanes.get(lane) ?? 0) + duration);
      const from = Date.parse(transition.recorded_at);
      laneIntervals.push([from, from + duration]);
    }
  }
  const intervals = attempts.flatMap((attempt) => laneIntervals.map(([from, to]) => [Math.max(from, Date.parse(attempt.started_at)), Math.min(to, attempt.status === "running" ? now : Date.parse(attempt.updated_at))] as const))
    .filter(([from, to]) => to > from).sort((a, b) => a[0] - b[0]);
  let working = 0;
  let through = start;
  for (const [from, to] of intervals) {
    working += Math.max(0, to - Math.max(from, through));
    through = Math.max(through, to);
  }
  const total = [...lanes.values()].reduce((sum, duration) => sum + duration, 0);
  return { lanes: [...lanes].map(([lane, duration]) => ({ lane, duration })), total, working: Math.min(total, working), system: Math.max(0, total - working), transitions: selected.length, start, end };
}

export function diagnosticsVerdict(issue: NativeIssue, explanation: IssueExplanation | null, latest: NativeAttempt | undefined) {
  if (issue.terminal) return { text: `This issue is ${issue.state}.`, action: "No further execution is scheduled.", variant: "success" as const };
  if (explanation === null) return { text: "Current state explanation unavailable.", action: "Try again when the issue evidence is readable.", variant: "secondary" as const };
  const decision = explanation.eligibility.current;
  const humanAction = explanation.required_gate.human_action || (latest?.disposition?.human_action && latest.disposition.status.toLowerCase() === issue.state.toLowerCase() ? latest.disposition.final_summary || "Human action requested" : "");
  const reason = explanation.reasons?.[0];
  const detail = (decision?.state === "refused" ? decision.reason : undefined) || reason?.detail || explanation.latest_transition?.reason || humanAction;
  return {
    text: `This issue is ${explanation.current_lane.name || issue.state}${detail ? `: ${detail.replace(/[.!]+$/, "")}` : ""}.`,
    action: humanAction || reason?.action || (latest?.status === "running" ? "The active attempt will report its outcome." : decision?.state === "eligible" ? "It can run when runner capacity is available." : "Resolution evidence unavailable."),
    variant: humanAction ? "error" as const : latest?.status === "running" ? "info" as const : "secondary" as const,
  };
}

export function durationLabel(milliseconds: number): string {
  const seconds = Math.max(0, Math.round(milliseconds / 1000));
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor(seconds % 3600 / 60)}m`;
}
