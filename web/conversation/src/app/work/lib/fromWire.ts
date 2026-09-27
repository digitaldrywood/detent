// The one place the hub's shapes become the board's.
//
// Everything the components read is built here, so a field that moves on the
// wire is a change to this file and nothing else. It is pure and synchronous:
// no fetching, no clock, no randomness.
import type {
  ChangeDetail,
  ChangeRequest,
  NativeAttempt,
  NativeIssue,
  NativeProject,
} from "../../../contracts/work.ts";
import { priorityName } from "../../../contracts/work.ts";
import type { AttemptView, ChangeView, Lane, ProjectView, WorkItemView } from "./model.ts";

/**
 * The lane category, derived from the two booleans the hub actually serves.
 * `NativeState` carries no category of its own, so this is the honest reading
 * of `terminal` and `dispatchable`: a dispatchable state is where work is
 * picked up, a terminal state is where it stops, and everything between is in
 * flight.
 */
function laneCategory(terminal: boolean, dispatchable: boolean): string {
  if (terminal) return "completed";
  if (dispatchable) return "unstarted";
  return "started";
}

export function toLanes(project: NativeProject): readonly Lane[] {
  return project.states.map((state) => ({
    // The hub gives states no id: the name is the identity, and it is what the
    // transition endpoint and the `state` filter both take.
    id: state.name,
    name: state.name,
    terminal: state.terminal,
    category: laneCategory(state.terminal, state.dispatchable),
  }));
}

/** The states this one is allowed to move to, in the workflow's own order. */
export function transitionsFrom(project: NativeProject, state: string): readonly string[] {
  const current = project.states.find((candidate) => candidate.name === state);
  const allowed = new Set(current?.transitions ?? []);
  return project.states
    .filter((candidate) => allowed.has(candidate.name))
    .map((candidate) => candidate.name);
}

export function toProjectView(
  project: NativeProject,
  items: readonly NativeIssue[],
): ProjectView {
  // The board's filter menus are built from the page it has loaded. Labels do
  // have a catalogue now (`GET {nativeBase}/labels`, read by the issue page's
  // label picker), but assignees and priorities do not, and a Filters menu
  // whose three lists came from two different places would say "every label
  // in the project" and "every assignee on this page" in the same column.
  // That is why the filter menus still read what is loaded, and why the
  // Filters menu says how many issues it read.
  const labels = new Set<string>();
  const assignees = new Set<string>();
  const priorities = new Set<string>();
  for (const item of items) {
    for (const label of item.labels) labels.add(label);
    for (const assignee of item.assignees) assignees.add(assignee);
    const priority = priorityName(item.priority);
    if (priority !== null) priorities.add(priority);
  }
  return {
    id: project.project_id,
    name: project.name,
    lanes: toLanes(project),
    labels: [...labels].toSorted(),
    assignees: [...assignees].toSorted(),
    priorities: [...priorities].toSorted(),
  };
}

/**
 * The attempt the worker strip describes: the newest one, which is the last
 * element because the hub orders attempts by ascending fencing token.
 *
 * `running` is the hub's own word, and it is already lease-checked: the read
 * path rewrites a `running` row to `interrupted` when its lease is gone. A
 * card that pulses is therefore claiming a live lease, not merely a row.
 */
export function toAttemptView(attempts: readonly NativeAttempt[]): AttemptView | null {
  const attempt = attempts.at(-1);
  if (attempt === undefined) return null;
  return {
    id: attempt.attempt_id,
    status: attempt.status,
    running: attempt.status === "running",
    runner: attempt.runner_id ?? attempt.machine_id ?? null,
    backend: attempt.identity?.backend ?? null,
    model: attempt.identity?.model ?? null,
    // The hub serves no effort or access on an attempt; the artifact's worker
    // strip shows them, and inventing them here would be decoration.
    effort: null,
    access: null,
    startedAt: attempt.started_at,
    // No token count and no progress exist anywhere on this resource, so the
    // card draws neither. See README, "What the hub does not serve".
    tokens: null,
    progress: null,
    attemptNumber: attempts.length,
  };
}

/** `succeeded`/`failed` and friends map onto the PR chip's review word. */
function reviewWord(summary: string): string {
  switch (summary) {
    case "ready":
    case "blocked":
    case "draft":
    case "merged":
      return summary;
    default:
      return summary.length > 0 ? summary : "";
  }
}

/**
 * The PR chip. The number and the link come from the current version's
 * external reference where the change was mirrored to GitHub; a change with no
 * external reference still gets a chip, named by its own version number,
 * because a review round exists either way.
 */
export function toChangeView(
  change: ChangeRequest,
  detail: ChangeDetail | null,
): ChangeView {
  const current =
    detail?.versions.find((version) => version.version_id === change.current_version_id) ??
    detail?.versions.at(-1) ??
    null;
  const external = current?.external ?? null;
  const number = external === null ? null : Number.parseInt(external.id, 10);
  return {
    id: change.change_id,
    number: number !== null && Number.isFinite(number) ? number : null,
    title: change.title,
    state: detail?.summary.status ?? "",
    url: external?.url ?? null,
    review: reviewWord(detail?.summary.status ?? ""),
  };
}

export interface ItemExtras {
  readonly attempts?: readonly NativeAttempt[];
  readonly change?: ChangeView | null;
  readonly conversationId?: string | null;
}

/**
 * `effort` and `epic` are labels by convention — `effort:medium`, `epic:3350`
 * — because the hub models neither as a field. They are read out of the label
 * list rather than guessed, and an issue with no such label simply has none.
 */
function labelValue(labels: readonly string[], prefix: string): string | null {
  const match = labels.find((label) => label.toLowerCase().startsWith(`${prefix}:`));
  return match === undefined ? null : match.slice(prefix.length + 1);
}

export function toWorkItemView(
  issue: NativeIssue,
  projectName: string,
  extras: ItemExtras = {},
): WorkItemView {
  const labels = [...issue.labels];
  return {
    id: issue.work_item_id,
    projectId: issue.project_id,
    projectName,
    identifier: `${projectName}#${issue.number}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    stateId: issue.state,
    priority: priorityName(issue.priority),
    labels,
    assignees: [...issue.assignees],
    effort: labelValue(labels, "effort"),
    epic: labelValue(labels, "epic"),
    createdAt: issue.created_at,
    updatedAt: issue.updated_at,
    revision: issue.revision,
    // Only a blocker that has not reached a terminal state still blocks. The
    // hub filters out blockers the reader cannot see, so this list is what
    // this reader is allowed to know about.
    blockedBy: issue.blockers
      .filter((blocker) => !blocker.terminal)
      .map((blocker) => blocker.work_item_id),
    attempt: extras.attempts === undefined ? null : toAttemptView(extras.attempts),
    change: extras.change ?? null,
    conversationId: extras.conversationId ?? null,
  };
}
