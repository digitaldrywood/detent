// The board's view model.
//
// The wire schemas in `src/contracts/work.ts` are the hub's shapes and are
// allowed to grow field by field. The board, the list and the issue page all
// read *this* instead, and one adapter (`fromWire.ts`) is the single place the
// two meet. That is not ceremony: the card has to render identically whether
// its attempt data came from the attempts endpoint, from the work item's own
// summary, or from nowhere at all, and pushing that decision into every
// component is how a board ends up with four different ideas of "running".
import type { WorkSort } from "./viewState.ts";

/** A workflow state, in the project's own order. */
export interface Lane {
  readonly id: string;
  readonly name: string;
  /**
   * A terminal state closes work. The artifact dims those lanes and collapses
   * them by default: they are evidence, not a queue.
   */
  readonly terminal: boolean;
  /** `backlog`, `unstarted`, `started`, `completed`, `cancelled`, or "". */
  readonly category: string;
}

/** A running or recently finished attempt, as the worker strip renders it. */
export interface AttemptView {
  readonly id: string;
  readonly status: string;
  readonly running: boolean;
  readonly runner: string | null;
  readonly backend: string | null;
  readonly model: string | null;
  readonly effort: string | null;
  readonly access: string | null;
  readonly startedAt: string | null;
  readonly tokens: number | null;
  /**
   * `0`–`1`, or `null` where the attempt reports no progress at all. The bar
   * is not drawn on a guess: A.1 records that the artifact's own progress rule
   * is dead CSS, so a bar here means the hub actually said something.
   */
  readonly progress: number | null;
  readonly attemptNumber: number | null;
}

/** A linked change request (a PR, in tracker terms), as the chip renders it. */
export interface ChangeView {
  readonly id: string;
  readonly number: number | null;
  readonly title: string;
  readonly state: string;
  readonly url: string | null;
  /** `blocked`, `ready`, `merged`, `draft`, `failing`, or "" when unknown. */
  readonly review: string;
}

export interface WorkItemView {
  readonly id: string;
  readonly projectId: string;
  readonly projectName: string;
  readonly identifier: string;
  readonly number: number | null;
  readonly title: string;
  readonly body: string;
  readonly state: string;
  readonly stateId: string;
  readonly priority: string | null;
  readonly labels: readonly string[];
  readonly assignees: readonly string[];
  readonly effort: string | null;
  readonly epic: string | null;
  readonly createdAt: string | null;
  readonly updatedAt: string | null;
  readonly revision: string | null;
  /** Dependencies that are not yet terminal. A non-empty list is "Blocked". */
  readonly blockedBy: readonly string[];
  readonly attempt: AttemptView | null;
  readonly change: ChangeView | null;
  readonly conversationId: string | null;
}

export interface ProjectView {
  readonly id: string;
  readonly name: string;
  readonly lanes: readonly Lane[];
  readonly labels: readonly string[];
  readonly priorities: readonly string[];
  readonly assignees: readonly string[];
}

/** True when the card should wear the live treatment (A.11: never a queue). */
export function isLive(item: WorkItemView): boolean {
  return item.attempt !== null && item.attempt.running;
}

export function isBlocked(item: WorkItemView): boolean {
  return item.blockedBy.length > 0;
}

const PRIORITY_ORDER: Readonly<Record<string, number>> = {
  urgent: 0,
  critical: 0,
  high: 1,
  normal: 2,
  medium: 2,
  low: 3,
  none: 4,
};

/** Lower sorts first. An unknown priority sorts after every known one. */
export function priorityRank(priority: string | null): number {
  if (priority === null) return 5;
  return PRIORITY_ORDER[priority.trim().toLowerCase()] ?? 5;
}

function time(at: string | null): number {
  if (at === null) return 0;
  const stamp = Date.parse(at);
  return Number.isNaN(stamp) ? 0 : stamp;
}

/**
 * The one ordering used by both views, so a card's position on the board and
 * its row in the list never disagree. Every comparison ends in the identifier,
 * so the order is total and a re-render cannot shuffle equal items.
 */
export function sortItems(
  items: readonly WorkItemView[],
  sort: WorkSort,
): readonly WorkItemView[] {
  const byIdentifier = (a: WorkItemView, b: WorkItemView) =>
    a.identifier.localeCompare(b.identifier, undefined, { numeric: true });
  return [...items].sort((a, b) => {
    switch (sort) {
      case "priority": {
        const rank = priorityRank(a.priority) - priorityRank(b.priority);
        if (rank !== 0) return rank;
        // Within a priority, a live card comes first: it is the one thing on
        // the lane that is changing while the reader looks at it.
        const live = Number(isLive(b)) - Number(isLive(a));
        if (live !== 0) return live;
        return byIdentifier(a, b);
      }
      case "updated": {
        const delta = time(b.updatedAt) - time(a.updatedAt);
        return delta !== 0 ? delta : byIdentifier(a, b);
      }
      case "created": {
        const delta = time(b.createdAt) - time(a.createdAt);
        return delta !== 0 ? delta : byIdentifier(a, b);
      }
      case "title": {
        const delta = a.title.localeCompare(b.title);
        return delta !== 0 ? delta : byIdentifier(a, b);
      }
    }
  });
}

/**
 * Client-side search over what is already loaded. The hub filters by state,
 * label, assignee and priority; it has no text search on work items, so this
 * is the honest fallback and the toolbar says it searches the loaded board.
 */
export function searchItems(
  items: readonly WorkItemView[],
  query: string,
): readonly WorkItemView[] {
  const needle = query.trim().toLowerCase();
  if (needle.length === 0) return items;
  return items.filter(
    (item) =>
      item.title.toLowerCase().includes(needle) ||
      item.identifier.toLowerCase().includes(needle) ||
      item.labels.some((label) => label.toLowerCase().includes(needle)),
  );
}

export interface BoardStats {
  readonly running: number;
  readonly ready: number;
  readonly waiting: number;
  readonly blocked: number;
  readonly completed: number;
  readonly total: number;
}

/**
 * The stats strip (A.1). Every counter is derived from the loaded issues, so
 * it cannot drift from the cards on screen: a strip that came from a separate
 * endpoint would sooner or later say "1 running" over an empty lane.
 */
export function boardStats(
  items: readonly WorkItemView[],
  lanes: readonly Lane[],
): BoardStats {
  const terminal = new Set(lanes.filter((lane) => lane.terminal).map((lane) => lane.name));
  const started = new Set(
    lanes.filter((lane) => lane.category === "started").map((lane) => lane.name),
  );
  let running = 0;
  let ready = 0;
  let waiting = 0;
  let blocked = 0;
  let completed = 0;
  for (const item of items) {
    if (terminal.has(item.state)) {
      completed += 1;
      continue;
    }
    if (isLive(item)) {
      running += 1;
      continue;
    }
    if (isBlocked(item)) {
      blocked += 1;
      continue;
    }
    if (started.has(item.state)) waiting += 1;
    else ready += 1;
  }
  return { running, ready, waiting, blocked, completed, total: items.length };
}
