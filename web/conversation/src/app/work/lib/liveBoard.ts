import { type NativeBoardCard, type NativeIssue, type NativeProject, type NativeWorkSummary } from "../../../contracts/work.ts";
import { invalidateQueries } from "../../lib/queryCache.ts";
import type { BoardView } from "./boardSchema.ts";
import { projectEventData, subscribeProjectQuery } from "./projectEvents.ts";
import { DEFAULT_VIEW_STATE } from "./viewState.ts";
import { BoardDeadlines, expireBoardEvidence, revalidateCachedLiveness } from "./boardLiveness.ts";
import { subscribeClock } from "./workClock.ts";
import type { WorkHttp } from "./workHttp.ts";

export const LIVE_DELTA_WINDOW_MS = 50;
export const LIVE_FALLBACK_MS = 60_000;
const LIVE_DELTA_MAX = 500;
const LIVE_CHECK_SETTLED_MS = 1_000;

export interface LiveEntry {
  readonly project: NativeProject;
  readonly cards: readonly NativeBoardCard[];
  readonly sequence: number;
  readonly grantVersion?: string | undefined;
  readonly work?: NativeWorkSummary | undefined;
}

export interface LiveState {
  readonly version: number;
  readonly resolved: boolean;
  readonly refreshing: boolean;
  readonly cached: boolean;
  readonly error: string | null;
  readonly asOf: number | null;
  readonly entries: ReadonlyMap<string, LiveEntry>;
  readonly failed: ReadonlyMap<string, string>;
  readonly countsPending: ReadonlySet<string>;
  readonly revoked: ReadonlySet<string>;
  readonly connected: boolean;
}

export interface LiveDeps {
  readonly overlay: (card: NativeBoardCard) => NativeBoardCard;
  readonly newer: (a: string | null, b: string | null) => boolean;
  readonly unauthorized: (cause: unknown) => boolean;
  readonly states: (projectId: string, project: NativeProject) => void;
  readonly restore: () => Promise<readonly LiveEntry[] | null>;
  readonly persist: (entries: readonly LiveEntry[], asOf: number) => void;
}

type ProjectQuery = ReturnType<typeof subscribeProjectQuery>;

const open = (card: NativeBoardCard) => !card.issue.terminal;
const hidden = () => typeof document !== "undefined" && document.visibilityState === "hidden";

export class LiveBoard {
  private readonly listeners = new Set<() => void>();
  private current: LiveState = { version: 0, resolved: false, refreshing: false, cached: false, error: null, asOf: null,
    entries: new Map(), failed: new Map(), countsPending: new Set(), revoked: new Set(), connected: false };
  private readonly queries = new Map<string, ProjectQuery>();
  private readonly connected = new Set<string>();
  private controller: AbortController | null = null;
  private generation = 0;
  private reloadScheduled = false;
  private reloadPending = false;
  private loadedAt = Number.NEGATIVE_INFINITY;
  private restoring = false;
  private readonly pendingItems = new Map<string, string>();
  private itemTimer: ReturnType<typeof setTimeout> | undefined;
  private itemsInFlight = false;
  private readonly itemGeneration = new Map<string, number>();
  private itemCounter = 0;
  private readonly deadlines = new BoardDeadlines();
  private releaseClock: (() => void) | undefined;
  private readonly workSequences = new Map<string, number>();
  private fallback: ReturnType<typeof setInterval> | undefined;
  private started = false;
  private disposed = false;

  constructor(readonly key: string, readonly http: WorkHttp, readonly scope: readonly string[], readonly archived: boolean,
    private readonly deps: LiveDeps) {}

  get state(): LiveState { return this.current; }

  get listening(): boolean { return this.listeners.size > 0; }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); };
  }

  start(): void {
    if (this.disposed) return;
    if (!this.started) {
      this.started = true;
      for (const id of this.scope) this.connect(id);
      if (typeof document !== "undefined") document.addEventListener("visibilitychange", this.visibility);
      this.releaseClock = subscribeClock(this.expireLiveness);
      this.watch();
    }
    if (this.controller === null && !this.reloadScheduled && (!this.current.resolved || this.current.error !== null)) this.reload();
  }

  reload = (): void => {
    if (this.disposed) return;
    if (this.controller !== null) { this.reloadPending = true; return; }
    if (this.reloadScheduled) return;
    this.reloadScheduled = true;
    queueMicrotask(() => {
      this.reloadScheduled = false;
      if (this.controller !== null) this.reloadPending = true;
      else this.load();
    });
  };

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.generation++;
    this.controller?.abort();
    this.controller = null;
    clearTimeout(this.itemTimer);
    clearInterval(this.fallback);
    this.fallback = undefined;
    this.pendingItems.clear();
    this.itemGeneration.clear();
    this.deadlines.clear();
    this.releaseClock?.();
    this.releaseClock = undefined;
    for (const query of this.queries.values()) query.close();
    this.queries.clear();
    this.connected.clear();
    if (typeof document !== "undefined") document.removeEventListener("visibilitychange", this.visibility);
  }

  revoke(projectId?: string): void {
    if (projectId !== undefined && !this.scope.includes(projectId)) return;
    const revoked = new Set(projectId === undefined ? this.scope : [...this.current.revoked, projectId]);
    if (revoked.size === this.scope.length) this.dispose();
    else {
      this.queries.get(projectId!)?.close();
      this.queries.delete(projectId!);
      this.connected.delete(projectId!);
      for (const [item, project] of this.pendingItems) if (project === projectId) this.pendingItems.delete(item);
      this.deadlines.forgetProject(projectId!);
      if (this.controller !== null) {
        this.controller.abort();
        this.controller = null;
        this.generation++;
        this.reloadPending = false;
        this.reload();
      }
    }
    const entries = new Map([...this.current.entries].filter(([id]) => !revoked.has(id)));
    this.publish({ ...this.current, entries, revoked, connected: this.isConnected() });
  }

  private publish(state: Omit<LiveState, "version">): void {
    this.current = { ...state, version: this.current.version + 1 };
    for (const listener of this.listeners) listener();
  }

  private requested(): string[] {
    return this.scope.filter((id) => !this.current.revoked.has(id));
  }

  private connect(id: string): void {
    const query = subscribeProjectQuery(this.http, id, {
      topics: ["work_item", "board", "lane", "activity"],
      apply: (event) => this.event(id, event),
      refresh: this.reload,
      restart: this.reload,
      connection: {
        open: () => { this.connected.add(id); this.watch(); },
        error: () => { this.connected.delete(id); this.watch(); },
        reset: () => { this.connected.delete(id); this.watch(); },
      },
    });
    this.queries.set(id, query);
  }

  private isConnected(): boolean {
    const requested = this.requested();
    return requested.length > 0 && requested.every((id) => this.connected.has(id));
  }

  private readonly visibility = (): void => {
    if (hidden()) this.connected.clear();
    this.watch();
    this.expireLiveness();
  };

  private watch(): void {
    const connected = this.isConnected();
    if (connected !== this.current.connected) this.publish({ ...this.current, connected });
    const polling = !this.disposed && !connected && this.requested().length > 0 && !hidden();
    if (polling && this.fallback === undefined) this.fallback = setInterval(this.check, LIVE_FALLBACK_MS);
    else if (!polling && this.fallback !== undefined) {
      clearInterval(this.fallback);
      this.fallback = undefined;
    }
  }

  private readonly expireLiveness = (): void => {
    if (hidden() || this.disposed) return;
    const due = this.deadlines.due();
    if (due.size === 0) return;
    const entries = new Map(this.current.entries);
    for (const [project, items] of due) {
      const entry = entries.get(project);
      if (entry === undefined) continue;
      entries.set(project, expireBoardEvidence(entry, (card) => items.has(card.issue.work_item_id)));
      for (const item of items) this.queue(project, item);
    }
    this.publish({ ...this.current, entries });
  };

  private readonly check = (): void => {
    if (this.controller !== null || this.reloadScheduled || Date.now() - this.loadedAt < LIVE_CHECK_SETTLED_MS) return;
    this.reload();
  };

  private event(projectId: string, event: Event): void {
    const settled = this.current.error === null && this.current.entries.has(projectId) && !this.current.failed.has(projectId) && !this.current.countsPending.has(projectId);
    if (event.type === "activity") {
      if (!settled) this.queries.get(projectId)?.refresh();
      return;
    }
    const item = event.type === "work_item" ? projectEventData(event).entity_id : undefined;
    if (settled && typeof item === "string" && item.length > 0) {
      invalidateQueries(["issue", item]);
      this.queue(projectId, item);
      return;
    }
    this.queries.get(projectId)?.refresh();
  }

  private queue(projectId: string, itemId: string): void {
    if (this.disposed || this.current.revoked.has(projectId)) return;
    this.pendingItems.set(itemId, projectId);
    if (this.itemTimer !== undefined) return;
    this.itemTimer = setTimeout(() => {
      this.itemTimer = undefined;
      void this.flush();
    }, LIVE_DELTA_WINDOW_MS);
  }

  confirm(issue: NativeIssue): void {
    const entry = this.current.entries.get(issue.project_id);
    if (this.disposed || entry === undefined) return;
    const previous = entry.cards.find((candidate) => candidate.issue.work_item_id === issue.work_item_id);
    if (this.deps.newer(previous?.issue.revision ?? null, issue.revision)) return;
    const cards = entry.cards.filter((candidate) => candidate !== previous);
    if (!issue.terminal) cards.push(previous === undefined ? { issue, attempt: null, change: null } : { ...previous, issue });
    this.itemGeneration.delete(issue.work_item_id);
    this.commit(new Map(this.current.entries).set(issue.project_id, { ...entry, cards }));
    this.queue(issue.project_id, issue.work_item_id);
  }

  private commit(entries: Map<string, LiveEntry>): void {
    this.publish({ ...this.current, entries, asOf: Date.now(), cached: false });
  }

  private save(): void {
    const requested = this.requested();
    if (this.current.asOf === null || this.current.failed.size > 0 || requested.some((id) => !this.current.entries.has(id))) return;
    this.deps.persist(requested.map((id) => this.current.entries.get(id)!), this.current.asOf);
  }

  private restore(generation: number): void {
    if (this.restoring) return;
    this.restoring = true;
    void this.deps.restore().then((entries) => {
      if (entries === null || this.disposed || this.current.resolved || generation !== this.generation) return;
      const requested = this.requested();
      if (entries.length !== requested.length || entries.some((entry, index) => entry.project.project_id !== requested[index])) return;
      const cached = entries.map(revalidateCachedLiveness);
      this.publish({ ...this.current, resolved: true, cached: true, asOf: Date.now(), entries: new Map(cached.map((entry) => [entry.project.project_id, entry])) });
    }, () => undefined);
  }

  private load(): void {
    const ids = this.requested();
    if (this.disposed || ids.length === 0) return;
    const controller = new AbortController();
    this.controller = controller;
    const generation = ++this.generation;
    clearTimeout(this.itemTimer);
    this.itemTimer = undefined;
    this.pendingItems.clear();
    this.itemGeneration.clear();
    const snapshots = new Map([...this.queries].map(([id, query]) => [id, query.beginSnapshot()]));
    this.publish({ ...this.current, refreshing: true });
    if (!this.current.resolved) this.restore(generation);
    void (async () => {
      try {
        const response = await this.http.readBoard({ projectIds: ids, state: { ...DEFAULT_VIEW_STATE, archived: this.archived || undefined }, signal: controller.signal });
        if (controller.signal.aborted || generation !== this.generation || [...snapshots.values()].some((snapshot) => !snapshot.current())) return;
        this.loadedAt = Date.now();
        this.publish(this.fromView(ids, response));
        this.expireLiveness();
        this.save();
        for (const [id, snapshot] of snapshots) {
          if (this.current.failed.has(id) || this.current.countsPending.has(id)) snapshot.fail();
          else snapshot.complete(this.current.entries.get(id)?.sequence ?? 0);
        }
      } catch (cause) {
        if (controller.signal.aborted || generation !== this.generation) return;
        for (const snapshot of snapshots.values()) snapshot.fail();
        if (this.deps.unauthorized(cause)) return;
        this.publish({ ...this.current, refreshing: false, error: cause instanceof Error ? cause.message : String(cause) });
      } finally {
        if (generation === this.generation) {
          this.controller = null;
          if (this.reloadPending) {
            this.reloadPending = false;
            this.reload();
          } else {
            if (this.current.refreshing) this.publish({ ...this.current, refreshing: false });
            if (this.pendingItems.size > 0 && this.itemTimer === undefined) void this.flush();
          }
        }
      }
    })();
  }

  private fromView(ids: readonly string[], response: BoardView): Omit<LiveState, "version"> {
    const projects = new Map(response.projects.map((entry) => [entry.project.project_id, entry]));
    const cards = new Map<string, NativeBoardCard[]>();
    for (const card of response.cards) {
      if (!open(card)) continue;
      const list = cards.get(card.issue.project_id);
      if (list === undefined) cards.set(card.issue.project_id, [this.deps.overlay(card)]);
      else list.push(this.deps.overlay(card));
    }
    const entries = new Map<string, LiveEntry>();
    const failed = new Map<string, string>();
    const countsPending = new Set<string>();
    for (const id of ids) {
      const entry = projects.get(id);
      const previous = this.current.entries.get(id);
      if (entry === undefined || entry.projection_pending) {
        failed.set(id, entry === undefined ? "Project is unavailable" : "Board projection is pending");
        if (previous !== undefined) entries.set(id, previous);
        continue;
      }
      this.deps.states(id, entry.project);
      this.workSequences.set(id, entry.sequence);
      if (entry.work === undefined) countsPending.add(id);
      const observed = cards.get(id) ?? [];
      this.deadlines.observe(observed);
      entries.set(id, { project: entry.project, cards: observed, sequence: entry.sequence, grantVersion: response.grant_version, work: entry.work });
    }
    this.deadlines.replace(entries.values());
    return { ...this.current, resolved: this.current.resolved || entries.size > 0 || failed.size === 0, refreshing: false, cached: false, error: null,
      asOf: Date.now(), entries, failed, countsPending };
  }

  private async flush(): Promise<void> {
    if (this.itemsInFlight || this.controller !== null || this.reloadScheduled || this.pendingItems.size === 0 || this.disposed) return;
    if (this.pendingItems.size > LIVE_DELTA_MAX) { this.reload(); return; }
    const batch = new Map(this.pendingItems);
    this.pendingItems.clear();
    const ids = [...batch.keys()];
    const generation = ++this.itemCounter;
    const projects = [...new Set(batch.values())].sort();
    for (const id of ids) this.itemGeneration.set(id, generation);
    this.itemsInFlight = true;
    try {
      const response = await this.http.readBoard({ projectIds: projects, state: { ...DEFAULT_VIEW_STATE, archived: this.archived || undefined }, items: ids });
      const current = ids.filter((id) => this.itemGeneration.get(id) === generation);
      if (!this.disposed && current.length > 0) this.applyItems(response, current, projects);
    } catch (cause) {
      if (!this.deps.unauthorized(cause)) for (const project of new Set(batch.values())) (this.queries.get(project)?.refresh ?? this.reload)();
    } finally {
      this.itemsInFlight = false;
      for (const id of ids) if (this.itemGeneration.get(id) === generation) this.itemGeneration.delete(id);
      if (this.pendingItems.size > 0 && this.itemTimer === undefined) void this.flush();
    }
  }

  private applyItems(response: BoardView, ids: readonly string[], requested: readonly string[]): void {
    const fresh = new Map(response.cards.map((card) => [card.issue.work_item_id, card]));
    const projects = new Map(response.projects.map((entry) => [entry.project.project_id, entry]));
    const unavailable = new Set(requested.filter((id) => projects.get(id) === undefined || projects.get(id)!.projection_pending));
    const entries = new Map(this.current.entries);
    const countsPending = new Set(this.current.countsPending);
    for (const [id, entry] of this.current.entries) {
      if (unavailable.has(id)) continue;
      let cards: NativeBoardCard[] | undefined;
      for (const itemId of ids) {
        const index = (cards ?? entry.cards).findIndex((card) => card.issue.work_item_id === itemId);
        const card = fresh.get(itemId);
        const existing = index < 0 ? undefined : (cards ?? entry.cards)[index];
        if (existing !== undefined && card !== undefined && this.deps.newer(existing.issue.revision, card.issue.revision)) continue;
        if (card !== undefined && unavailable.has(card.issue.project_id) && existing !== undefined && card.issue.project_id !== id) continue;
        const belongs = card !== undefined && card.issue.project_id === id && open(card);
        if (existing === undefined && !belongs) continue;
        cards ??= [...entry.cards];
        if (index >= 0) cards.splice(index, 1);
        this.deadlines.forget(itemId);
        if (belongs) {
          const observed = this.deps.overlay(card);
          cards.push(observed);
          this.deadlines.observe([observed]);
          this.deadlines.track(observed);
        }
      }
      const update = projects.get(id);
      const counted = update !== undefined && update.sequence >= (this.workSequences.get(id) ?? 0);
      const work = counted && update.work !== undefined && !(cards ?? entry.cards).some((card) => card.liveness_expires_at === null) ? update.work : entry.work;
      if (counted) {
        this.workSequences.set(id, update.sequence);
        if (update.work === undefined) countsPending.add(id);
        else countsPending.delete(id);
      }
      if (cards !== undefined || work !== entry.work) entries.set(id, { ...entry, cards: cards ?? entry.cards, work });
    }
    this.publish({ ...this.current, entries, countsPending, asOf: Date.now(), cached: false });
    for (const id of unavailable) (this.queries.get(id)?.refresh ?? this.reload)();
  }
}

export function liveCompleted(work: NativeWorkSummary, window: string): { completed: number | undefined; cancelled: number | undefined } {
  const selected = work.shipped_windows?.find((entry) => entry.window === window);
  return selected === undefined ? { completed: work.completed, cancelled: work.cancelled } : { completed: selected.shipped, cancelled: selected.cancelled };
}
