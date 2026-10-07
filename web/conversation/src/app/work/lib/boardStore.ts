import * as Schema from "effect/Schema";

import { NativeIssue, NativeProject, NativeWorkSummary, priorityValue, type NativeAttempt } from "../../../contracts/work.ts";
import { BOARD_CACHE_LIMIT, BOARD_CACHE_VERSION, readBoardDisk, updateBoardDisk } from "./boardDisk.ts";
import { toChangeView, toLanes, toProjectView, toWorkItemView } from "./fromWire.ts";
import type { Lane, ProjectView, ScopedWorkStats, WorkItemView } from "./model.ts";
import type { WorkHttp } from "./workHttp.ts";
import { effectiveSort, tabMatches, tabQuery, type WorkViewState } from "./viewState.ts";

export interface BoardData {
  readonly resolved: boolean;
  readonly refreshing: boolean;
  readonly cached: boolean;
  readonly totals: ScopedWorkStats | null;
  readonly hasMore: boolean;
  readonly backlogHasMore: boolean;
  readonly backlogLoading: boolean;
  readonly backlogTotal: number;
  readonly loading: boolean;
  readonly error: string | null;
  readonly project: ProjectView | null;
  readonly workflows: readonly NativeProject[];
  readonly lanes: readonly Lane[];
  readonly items: readonly WorkItemView[];
  readonly labels: readonly string[];
  readonly assignees: readonly string[];
  readonly priorities: readonly string[];
  readonly truncated: boolean;
  readonly enriched: number;
  readonly asOf: number | null;
}

const EMPTY: BoardData = {
  resolved: false, refreshing: false, cached: false, totals: null, hasMore: false, backlogHasMore: false, backlogLoading: false, backlogTotal: 0,
  loading: true, error: null, project: null, workflows: [], lanes: [], items: [],
  labels: [], assignees: [], priorities: [], truncated: false, enriched: 0, asOf: null,
};

const LoadedSchema = Schema.Struct({
  project: NativeProject,
  issues: Schema.Array(NativeIssue),
  work: Schema.optional(NativeWorkSummary),
  nextCursor: Schema.optional(Schema.String),
  pageCount: Schema.Number,
  backlogCursor: Schema.optional(Schema.String),
  backlogPageCount: Schema.Number,
  backlogTotal: Schema.Number,
});
type Loaded = typeof LoadedSchema.Type;

async function pooled<A, B>(inputs: readonly A[], work: (input: A) => Promise<B>): Promise<B[]> {
  const results: B[] = new Array(inputs.length);
  let next = 0;
  await Promise.all(Array.from({ length: Math.min(6, inputs.length) }, async () => {
    for (;;) {
      const index = next++;
      if (index >= inputs.length) return;
      results[index] = await work(inputs[index]!);
    }
  }));
  return results;
}

function unauthorized(cause: unknown): boolean {
  return typeof cause === "object" && cause !== null && "status" in cause && [401, 403].includes(Number(cause.status));
}

function newer(a: string | null, b: string | null): boolean {
  if (a === null || b === null) return false;
  try { return BigInt(a) > BigInt(b); } catch { return false; }
}

function enrichable(loaded: readonly Loaded[]): NativeIssue[] {
  const operationalOrder = new Map(loaded.flatMap((entry) => (entry.work?.items ?? [])
      .map((issue, index) => [issue.work_item_id, index] as const)));
  return [...new Map(loaded.flatMap((entry) => entry.issues).map((issue) => [issue.work_item_id, issue])).values()]
      .filter((issue) => !issue.terminal).toSorted((a, b) =>
        (operationalOrder.get(a.work_item_id) ?? Infinity) - (operationalOrder.get(b.work_item_id) ?? Infinity) ||
        Date.parse(b.updated_at) - Date.parse(a.updated_at)).slice(0, 24);
}

export interface BoardAccount {
  readonly http: { readonly origin: string; readonly apiBase: string };
  readonly bootstrap: {
    readonly organization: { readonly id: string };
    readonly actor?: { readonly principal_id: string };
    readonly projects: readonly { readonly id: string; readonly can_write?: boolean }[];
  };
}

export function boardAccountKey(client: BoardAccount): string {
  return JSON.stringify([BOARD_CACHE_VERSION, client.http.origin || globalThis.location?.origin || "",
    client.http.apiBase, client.bootstrap.organization.id, client.bootstrap.actor?.principal_id ?? null,
    client.bootstrap.projects.map((project) => [project.id, project.can_write === true]).sort()]);
}

let account: string | null = null;
let epoch = 0;
let mutationVersion = 0;
const boards = new Map<string, BoardRead>();
const metadata = new Map<string, { at: number; read: Promise<NativeProject> }>();
const confirmed = new Map<string, NativeIssue>();
const facets = new Map<string, Pick<ProjectView, "labels" | "assignees" | "priorities">>();

export function authorizeBoardCache(next: string): void {
  if (next === account) return;
  clearMemory();
  account = next;
  const current = epoch;
  void updateBoardDisk(next, undefined, () => epoch === current);
}

function clearMemory(): void {
  epoch++;
  account = null;
  metadata.clear();
  confirmed.clear();
  facets.clear();
  for (const board of boards.values()) board.revoke();
  boards.clear();
}

export function clearBoardCache(): void {
  clearMemory();
  const current = epoch;
  void updateBoardDisk(null, undefined, () => epoch === current);
}

export function rejectBoardCache(owner: string | null = account): void {
  if (owner !== account) return;
  epoch++;
  metadata.clear();
  confirmed.clear();
  facets.clear();
  for (const board of boards.values()) board.revoke();
  const current = epoch;
  void updateBoardDisk(null, undefined, () => epoch === current);
}

function getProject(http: WorkHttp, id: string): Promise<NativeProject> {
  const previous = metadata.get(id);
  if (previous !== undefined && Date.now() - previous.at < 60_000) return previous.read;
  const read = http.getProject(id).catch((cause) => {
    if (metadata.get(id)?.read === read) metadata.delete(id);
    throw cause;
  });
  metadata.set(id, { at: Date.now(), read });
  return read;
}

function normalized(view: WorkViewState): WorkViewState {
  const values = (entries: readonly string[]) => [...new Set(entries)].sort();
  return { ...view, q: view.q.trim(), state: view.view === "list" ? [] : values(view.state), label: values(view.label),
    assignee: values(view.assignee), priority: values(view.priority), lanes: view.lanes === null ? null : values(view.lanes) };
}

export function getBoardRead(client: BoardAccount, http: WorkHttp, projectId: string | null, input: WorkViewState): BoardRead {
  const owner = boardAccountKey(client);
  authorizeBoardCache(owner);
  const accessible = client.bootstrap.projects.map((project) => project.id).sort();
  const scope = projectId === null ? accessible : accessible.includes(projectId) ? [projectId] : [];
  const view = normalized(input);
  const key = JSON.stringify([owner, projectId, scope, view.q, view.state, view.label, view.assignee,
    view.priority.map((name) => String(priorityValue(name) ?? name)).sort(), view.archived === true, view.completedWindow,
    view.view, view.view === "list" ? view.tab : view.lanes, effectiveSort(view) === "closed"]);
  let board = boards.get(key);
  if (board !== undefined) {
    boards.delete(key);
    boards.set(key, board);
    return board;
  }
  board = new BoardRead(owner, key, http, projectId, scope, view);
  boards.set(key, board);
  for (const [oldKey, old] of boards) {
    if (boards.size <= BOARD_CACHE_LIMIT) break;
    if (old.subscribers.size > 0) continue;
    old.dispose();
    boards.delete(oldKey);
  }
  return board;
}

export function confirmWorkItem(issue: NativeIssue, owner: string | null = account): void {
  if (account === null || owner !== account || newer(confirmed.get(issue.work_item_id)?.revision ?? null, issue.revision)) return;
  confirmed.set(issue.work_item_id, issue);
  mutationVersion++;
  for (const board of boards.values()) board.confirm(issue);
}

export class BoardRead {
  readonly subscribers = new Set<() => void>();
  private state: BoardData = EMPTY;
  private entries: readonly Loaded[] = [];
  private controller: AbortController | null = null;
  private enrichment: AbortController | null = null;
  private generation = 0;
  private pending = false;
  private continuation = false;
  private revoked = false;
  private readonly facetKey: string;

  constructor(readonly owner: string, readonly key: string, readonly http: WorkHttp,
    readonly projectId: string | null, readonly scope: readonly string[], readonly view: WorkViewState) {
    this.facetKey = JSON.stringify([owner, scope, view.archived === true]);
    if (scope.length === 0) this.state = { ...EMPTY, loading: false, resolved: true };
    else {
      const previous = [...boards.values()].find((board) => board.facetKey === this.facetKey && board.state.resolved);
      if (previous !== undefined) this.state = { ...EMPTY, lanes: previous.state.lanes,
        workflows: previous.state.workflows, project: previous.state.project, ...facets.get(this.facetKey) };
    }
  }

  snapshot = (): BoardData => this.state;

  subscribe = (notify: () => void): (() => void) => {
    this.subscribers.add(notify);
    return () => {
      this.subscribers.delete(notify);
      if (this.subscribers.size === 0 && this.continuation) this.dispose();
    };
  };

  private publish(state: BoardData): void {
    this.state = state;
    for (const notify of this.subscribers) notify();
  }

  dispose(): void {
    this.generation++;
    this.controller?.abort();
    this.enrichment?.abort();
    this.controller = null;
    this.enrichment = null;
    this.pending = false;
  }

  revoke(): void {
    this.revoked = true;
    this.dispose();
    this.entries = [];
    this.state = { ...EMPTY, loading: false, error: "Access to the board is no longer available." };
    queueMicrotask(() => { for (const notify of this.subscribers) notify(); });
  }

  private persist(): void {
    if (this.entries.length === 0 || this.state.asOf === null) return;
    const current = epoch;
    void updateBoardDisk(this.owner, { key: this.key, account: this.owner, version: BOARD_CACHE_VERSION,
      asOf: this.state.asOf!, entries: this.entries }, () => epoch === current && !this.revoked);
  }

  private async restore(generation: number): Promise<void> {
    const mutations = mutationVersion;
    const stored = await readBoardDisk(this.key);
    if (this.revoked || generation !== this.generation || mutations !== mutationVersion || this.state.resolved || stored === null ||
      stored.account !== this.owner || stored.version !== BOARD_CACHE_VERSION || !Number.isFinite(stored.asOf)) return;
    try {
      const entries = Schema.decodeUnknownSync(Schema.Array(LoadedSchema))(stored.entries);
      if (entries.length !== this.scope.length || entries.some((entry, index) => entry.project.project_id !== this.scope[index] ||
        !Number.isInteger(entry.pageCount) || entry.pageCount < 1 ||
        !Number.isInteger(entry.backlogPageCount) || entry.backlogPageCount < 0 ||
        !Number.isInteger(entry.backlogTotal) || entry.backlogTotal < 0 ||
        [...entry.issues, ...(entry.work?.items ?? [])].some((issue) => issue.project_id !== entry.project.project_id))) return;
      this.entries = entries;
      this.publish({ ...this.build(entries, stored.asOf), cached: true, refreshing: this.controller !== null, error: this.state.error });
    } catch {
      return;
    }
  }

  private async loadProject(id: string, signal: AbortSignal, previous?: Loaded, continuing = false): Promise<Loaded> {
    const project = await getProject(this.http, id);
    const board = this.view.view === "board";
    if (!board) {
      if (continuing && previous?.nextCursor === undefined) return previous!;
      const filters = {
        projectId: id, includeWork: true, completedWindow: this.view.completedWindow, q: this.view.q,
        archived: this.view.archived === true, label: this.view.label, assignee: this.view.assignee,
        priority: this.view.priority.map((name) => String(priorityValue(name) ?? name)), signal,
      };
      const issues = continuing ? [...(previous?.issues ?? [])] : [];
      let cursor = continuing ? previous?.nextCursor : undefined;
      let pageCount = continuing ? previous!.pageCount : 0;
      let work: Loaded["work"];
      do {
        signal.throwIfAborted();
        const [page, totals] = await Promise.all([
          this.http.listWorkItems({ ...filters, limit: 100, ...tabQuery(this.view, toLanes(project)),
            sort: effectiveSort(this.view) === "closed" ? "closed" : undefined, cursor }),
          this.http.listWorkItems({ ...filters, limit: 1 }),
        ]);
        issues.push(...page.items);
        work = totals.work ?? page.work;
        cursor = page.next_cursor;
        pageCount++;
      } while (cursor !== undefined && !continuing &&
        (pageCount < (previous?.pageCount ?? 1) || ["active", "backlog"].includes(this.view.tab)));
      return { project, issues, work, nextCursor: cursor, pageCount,
        backlogPageCount: 0, backlogTotal: 0 };
    }
    const states = project.states.filter((state) => !state.terminal &&
      (this.view.lanes === null || this.view.lanes.includes(state.name)) &&
      (this.view.state.length === 0 || this.view.state.includes(state.name)));
    const active = states.filter((state) => state.name.toLowerCase() !== "backlog").map((state) => state.name);
    const backlog = states.find((state) => state.name.toLowerCase() === "backlog");
    const filters = {
      projectId: id, completedWindow: this.view.completedWindow, q: this.view.q,
      archived: this.view.archived === true, label: this.view.label, assignee: this.view.assignee,
      priority: this.view.priority.map((name) => String(priorityValue(name) ?? name)), signal,
    };
    const readPage = (state: readonly string[], cursor?: string) =>
      this.http.listWorkItems({ ...filters, limit: 200, open: true, state, cursor });
    if (continuing) {
      if (previous?.backlogCursor === undefined || backlog === undefined) return previous!;
      const page = await readPage([backlog.name], previous.backlogCursor);
      return { ...previous, issues: [...previous.issues, ...page.items], backlogCursor: page.next_cursor,
        backlogPageCount: previous.backlogPageCount + 1 };
    }
    const totals = this.http.listWorkItems({ ...filters, limit: 1, includeWork: true });
    totals.catch(() => undefined);
    const issues: NativeIssue[] = [];
    let cursor: string | undefined;
    let pageCount = 0;
    if (active.length > 0) {
      do {
        signal.throwIfAborted();
        const page = await readPage(active, cursor);
        issues.push(...page.items);
        cursor = page.next_cursor;
        pageCount++;
      } while (cursor !== undefined);
    }
    const work = (await totals).work;
    let backlogCursor: string | undefined;
    let backlogPageCount = 0;
    let backlogTotal = 0;
    if (backlog !== undefined) {
      do {
        signal.throwIfAborted();
        const page = await readPage([backlog.name], backlogCursor);
        backlogTotal = page.total ?? work?.lanes.find((lane) => lane.state === backlog.name)?.total ?? backlogTotal;
        issues.push(...page.items);
        backlogCursor = page.next_cursor;
        backlogPageCount++;
      } while (backlogCursor !== undefined && backlogPageCount < (previous?.backlogPageCount ?? 1));
    }
    return { project, issues, work, nextCursor: cursor, pageCount: Math.max(1, pageCount),
      backlogCursor, backlogPageCount, backlogTotal };
  }

  private matches(issue: NativeIssue, project: NativeProject): boolean {
    const view = this.view;
    const q = view.q.toLowerCase();
    return (view.view !== "board" || (!issue.terminal && project.states.some((state) => state.name === issue.state && !state.terminal) &&
      (view.lanes === null || view.lanes.includes(issue.state)))) && (issue.archived === true) === (view.archived === true) &&
      (view.view === "list" ? tabMatches(view.tab, { state: issue.state, terminal: project.states.find((state) => state.name === issue.state)?.terminal ?? issue.terminal })
        : view.state.length === 0 || view.state.includes(issue.state)) &&
      (view.label.length === 0 || view.label.some((label) => issue.labels.includes(label))) &&
      (view.assignee.length === 0 || view.assignee.some((assignee) => issue.assignees.includes(assignee))) &&
      (view.priority.length === 0 || view.priority.some((name) => String(priorityValue(name) ?? name) === String(issue.priority))) &&
      (q.length === 0 || [issue.title, `${project.name}#${issue.number}`, `${project.project_id}#${issue.number}`, ...issue.labels]
        .some((value) => value.toLowerCase().includes(q)));
  }

  reload = (): void => {
    if (this.controller !== null) { this.pending = true; return; }
    this.revoked = false;
    this.read(false);
  };

  start = (): void => {
    if (this.controller === null) this.reload();
  };

  loadMore = (): void => {
    if (this.view.view === "board" || !this.state.hasMore || this.continuation) return;
    this.dispose();
    this.read(true);
  };

  loadBacklog = (): void => {
    if (!this.state.backlogHasMore || this.continuation) return;
    this.dispose();
    this.read(true);
  };

  private read(continuing: boolean): void {
    if (this.revoked || this.scope.length === 0) return;
    this.enrichment?.abort();
    const controller = new AbortController();
    this.controller = controller;
    this.continuation = continuing;
    const signal = controller.signal;
    const generation = ++this.generation;
    let prior = this.entries;
    this.publish({ ...this.state, loading: (!this.state.resolved || (continuing && this.view.view !== "board")),
      backlogLoading: continuing && this.view.view === "board", refreshing: true, error: null });
    if (!this.state.resolved) void this.restore(generation);
    void (async () => {
      try {
        const loaded = await pooled(this.scope, async (id) => {
          if (!continuing && prior.length === 0 && this.entries.length > 0) prior = this.entries;
          const previous = prior.find((entry) => entry.project.project_id === id);
          const entry = await this.loadProject(id, signal, previous, continuing);
          const issues = entry.issues;
          let changed = false;
          const previousItems = new Map(this.entries.flatMap((entry) => entry.issues).map((issue) => [issue.work_item_id, issue]));
          const current = [...new Map(issues.map((issue) => [issue.work_item_id, issue])).values()]
            .map((issue) => {
              const mutation = confirmed.get(issue.work_item_id);
              const previous = previousItems.get(issue.work_item_id);
              const latest = newer(previous?.revision ?? null, mutation?.revision ?? null) ? previous : mutation ?? previous;
              if (latest === undefined || !newer(latest.revision, issue.revision)) return issue;
              changed = true;
              return latest;
            }).filter((issue) => this.matches(issue, entry.project));
          const ids = new Set(current.map((issue) => issue.work_item_id));
          for (const issue of confirmed.values()) {
            if (issue.project_id !== id || ids.has(issue.work_item_id) || !this.matches(issue, entry.project)) continue;
            current.push(issue);
            changed = true;
          }
          return { ...entry, issues: current, work: changed ? undefined : entry.work };
        });
        if (signal.aborted || generation !== this.generation) return;
        this.entries = loaded;
        this.publish(this.build(loaded, Date.now()));
        this.persist();
        this.enrich(loaded, generation);
      } catch (cause) {
        if (signal.aborted || generation !== this.generation) return;
        if (unauthorized(cause)) rejectBoardCache(this.owner);
        else this.publish({ ...this.state, loading: false, backlogLoading: false, refreshing: false,
          error: cause instanceof Error ? cause.message : String(cause) });
      } finally {
        if (generation !== this.generation) return;
        this.controller = null;
        this.continuation = false;
        if (this.pending) { this.pending = false; this.reload(); }
      }
    })();
  }

  private build(loaded: readonly Loaded[], asOf: number): BoardData {
    const lanes: Lane[] = [];
    const seen = new Set<string>();
    for (const entry of loaded) {
      for (const lane of toProjectView(entry.project, entry.issues).lanes) {
        if (seen.has(lane.name)) continue;
        seen.add(lane.name);
        lanes.push(lane);
      }
    }
    let totals: ScopedWorkStats | null = null;
    if (loaded.every((entry) => entry.work !== undefined)) {
      const counts = Object.create(null) as Record<string, number>;
      let running = 0, queued = 0, closed = 0, total = 0;
      for (const entry of loaded) {
        for (const lane of entry.work!.lanes) {
          const state = entry.project.states.find((state) => state.name === lane.state);
          counts[lane.state] = (counts[lane.state] ?? 0) + lane.total;
          total += lane.total;
          running += lane.running;
          if (state?.terminal) closed += lane.total;
          else if (state?.dispatchable || lane.state.toLowerCase() === "backlog") queued += lane.total - lane.running;
        }
      }
      if (this.view.view === "board") {
        for (const entry of loaded) {
          const backlog = entry.project.states.find((state) => state.name.toLowerCase() === "backlog");
          if (backlog === undefined || entry.work!.lanes.some((lane) => lane.state === backlog.name)) continue;
          counts[backlog.name] = (counts[backlog.name] ?? 0) + entry.backlogTotal;
          total += entry.backlogTotal;
          queued += entry.backlogTotal;
        }
      }
      const completed = loaded.every((entry) => entry.work!.completed !== undefined)
        ? loaded.reduce((count, entry) => count + entry.work!.completed!, 0)
        : this.view.completedWindow === "all" ? closed : null;
      totals = { lanes: counts, running, queued, open: total - closed, completed, total,
        asOf: loaded.map((entry) => entry.work!.as_of).toSorted()[0]!,
        truncated: loaded.some((entry) => entry.work!.truncated) };
    }
    const issues = [...new Map(loaded.flatMap((entry) => entry.issues).map((issue) => [issue.work_item_id, issue])).values()];
    const selected = new Set(enrichable(loaded).map((issue) => issue.work_item_id));
    const items = issues.map((issue) => {
      const project = loaded.find((entry) => entry.project.project_id === issue.project_id)!.project;
      const item = toWorkItemView(issue, project.name);
      const previous = this.state.items.find((candidate) => candidate.id === item.id);
      return { ...item, reserveWorkerSpace: selected.has(item.id), terminal: project.states.find((state) => state.name === issue.state)?.terminal ?? issue.terminal,
        ...(previous?.revision === item.revision ? { attempt: previous.attempt, change: previous.change, observations: previous.observations } : {}) };
    });
    const current = toProjectView(loaded[0]!.project, issues);
    const previous = facets.get(this.facetKey);
    const choices = {
      labels: [...new Set([...(previous?.labels ?? []), ...current.labels])].sort(),
      assignees: [...new Set([...(previous?.assignees ?? []), ...current.assignees])].sort(),
      priorities: [...new Set([...(previous?.priorities ?? []), ...current.priorities])].sort(),
    };
    facets.set(this.facetKey, choices);
    return { resolved: true, refreshing: false, cached: false, loading: false, error: null,
      totals, hasMore: this.view.view !== "board" && loaded.some((entry) => entry.nextCursor !== undefined),
      backlogHasMore: loaded.some((entry) => entry.backlogCursor !== undefined), backlogLoading: false,
      backlogTotal: loaded.reduce((total, entry) => total + entry.backlogTotal, 0),
      project: this.projectId === null ? null : { ...current, ...choices, lanes },
      workflows: loaded.map((entry) => entry.project), lanes, items, ...choices,
      truncated: loaded.some((entry) => entry.nextCursor !== undefined || entry.backlogCursor !== undefined), enriched: 0, asOf };
  }

  private enrich(loaded: readonly Loaded[], generation: number): void {
    const controller = new AbortController();
    this.enrichment = controller;
    const signal = controller.signal;
    const issues = enrichable(loaded);
    void pooled(issues, async (issue) => {
      signal.throwIfAborted();
      const [attemptRead, changeRead] = await Promise.allSettled([
        this.http.listAttempts(issue.project_id, issue.work_item_id, 10, signal),
        this.http.listChanges(issue.project_id, issue.work_item_id, signal),
      ]);
      signal.throwIfAborted();
      for (const read of [attemptRead, changeRead]) if (read.status === "rejected" && unauthorized(read.reason)) throw read.reason;
      const change = changeRead.status === "fulfilled" ? changeRead.value.at(-1) : undefined;
      let detail = null;
      if (change !== undefined) {
        try { detail = await this.http.getChange(issue.project_id, issue.work_item_id, change.change_id, signal); }
        catch (cause) { if (unauthorized(cause)) throw cause; }
      }
      if (signal.aborted || generation !== this.generation) return;
      const attempts: readonly NativeAttempt[] = attemptRead.status === "fulfilled" ? attemptRead.value.items : [];
      const extra = toWorkItemView(issue, "", { attempts, change: change === undefined ? null : toChangeView(change, detail),
        observations: { worker: attemptRead.status === "rejected" ? "unavailable" : attemptRead.value.next_cursor === undefined ? "known" : "partial",
          change: changeRead.status === "rejected" ? "unavailable" : change !== undefined && detail === null ? "partial" : "known" } });
      this.publish({ ...this.state, enriched: this.state.enriched + 1, items: this.state.items.map((item) =>
        item.id === issue.work_item_id && item.revision === issue.revision
          ? { ...item, attempt: extra.attempt, change: extra.change, observations: extra.observations } : item) });
    }).catch((cause) => {
      if (!signal.aborted && generation === this.generation && unauthorized(cause)) rejectBoardCache(this.owner);
    });
  }

  confirm(issue: NativeIssue): void {
    if (!this.scope.includes(issue.project_id)) return;
    this.entries = this.entries.map((entry) => {
      if (entry.project.project_id !== issue.project_id) return entry;
      const previous = entry.issues.find((candidate) => candidate.work_item_id === issue.work_item_id);
      if (newer(previous?.revision ?? null, issue.revision)) return entry;
      const issues = entry.issues.filter((candidate) => candidate.work_item_id !== issue.work_item_id);
      if (this.matches(issue, entry.project)) issues.push(issue);
      return { ...entry, issues, work: undefined };
    });
    if (!this.state.resolved) return;
    this.publish({ ...this.build(this.entries, this.state.asOf!), refreshing: this.state.refreshing });
    this.persist();
  }

  applyItem = (item: WorkItemView): void => {
    const issue = this.entries.flatMap((entry) => entry.issues).find((candidate) => candidate.work_item_id === item.id);
    if (issue === undefined || item.revision === null) return;
    confirmWorkItem({ ...issue, state: item.state, revision: item.revision, terminal: item.terminal,
      archived: item.archived, title: item.title, body: item.body, labels: item.labels, assignees: item.assignees });
  };
}
