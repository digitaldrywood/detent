import type { NativeBoardCard, NativeWorkSummary } from "../../../contracts/work.ts";

type BoardEvidence = {
  readonly cards?: readonly NativeBoardCard[] | undefined;
  readonly work?: NativeWorkSummary | undefined;
};

export class BoardDeadlines {
  private readonly received = new WeakMap<NativeBoardCard, number>();
  private readonly active = new Map<string, { project: string; at: number }>();

  observe(cards: readonly NativeBoardCard[]): void {
    const receivedAt = performance.now();
    for (const card of cards) {
      if (!card.liveness_expires_at || !card.liveness_observed_at) continue;
      const remaining = Date.parse(card.liveness_expires_at) - Date.parse(card.liveness_observed_at);
      if (Number.isFinite(remaining)) this.received.set(card, receivedAt + Math.max(0, remaining));
    }
  }

  track(card: NativeBoardCard): void {
    const at = this.received.get(card);
    if (at !== undefined) this.active.set(card.issue.work_item_id, { project: card.issue.project_id, at });
  }

  replace(entries: Iterable<BoardEvidence>): void {
    this.clear();
    for (const entry of entries) for (const card of entry.cards ?? []) this.track(card);
  }

  forget(item: string): void {
    this.active.delete(item);
  }

  forgetProject(project: string): void {
    for (const [item, observation] of this.active) if (observation.project === project) this.forget(item);
  }

  clear(): void {
    this.active.clear();
  }

  due(): Map<string, Set<string>> {
    const now = performance.now();
    const projects = new Map<string, Set<string>>();
    for (const [item, { project, at }] of this.active) {
      if (now < at) continue;
      this.forget(item);
      const items = projects.get(project) ?? new Set<string>();
      items.add(item);
      projects.set(project, items);
    }
    return projects;
  }
}

export function runningBoardCard(card: NativeBoardCard): boolean {
  return card.liveness_expires_at !== null && (card.execution?.running ?? (card.attempt?.status === "running"));
}

export function expireBoardEvidence<T extends BoardEvidence>(entry: T, expired: (card: NativeBoardCard) => boolean): T {
  let changed = false;
  const running = new Map<string, number>();
  const cards = entry.cards?.map((card) => {
    if (card.liveness_expires_at === null || !expired(card)) return card;
    changed = true;
    const lane = card.issue.state;
    if (runningBoardCard(card)) running.set(lane, (running.get(lane) ?? 0) + 1);
    return { ...card, liveness_expires_at: null, liveness_observed_at: undefined, execution: undefined, attempt: null, status_line: undefined };
  });
  if (!changed) return entry;
  const work = entry.work === undefined || running.size === 0 ? entry.work : {
    ...entry.work,
    lanes: entry.work.lanes.map((lane) => ({ ...lane, running: Math.max(0, lane.running - (running.get(lane.state) ?? 0)) })),
  };
  return { ...entry, cards, work };
}

export function revalidateCachedLiveness<T extends BoardEvidence>(entry: T): T {
  return expireBoardEvidence(entry, (card) =>
    card.liveness_expires_at !== undefined || runningBoardCard(card) || Boolean(card.execution?.stage));
}
