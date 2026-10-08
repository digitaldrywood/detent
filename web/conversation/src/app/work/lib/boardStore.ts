import * as Schema from "effect/Schema";

import { NativeIssue, NativeProject, NativeWorkSummary, priorityValue, NativeBoardCard, type NativeBoardFrame } from "../../../contracts/work.ts";
import { BOARD_CACHE_LIMIT, BOARD_CACHE_VERSION, readBoardDisk, updateBoardDisk } from "./boardDisk.ts";
import { toBoardCardView, toLanes, toProjectView, toWorkItemView } from "./fromWire.ts";
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
  cards: Schema.optional(Schema.Array(NativeBoardCard)),
  sequence: Schema.optional(Schema.Number),
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
  private readonly frames = new Map<string, NativeBoardFrame[]>();
  private readonly views = new Map<string, { issue: NativeIssue; card: NativeBoardCard | undefined; item: WorkItemView }>();
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
    this.controller = null;
    this.pending = false;
  }

  revoke(): void {
    this.revoked = true;
    this.dispose();
    this.entries = [];
    this.frames.clear();
    this.views.clear();
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
        projectId: id, includeBoard: true, completedWindow: this.view.completedWindow, q: this.view.q,
        archived: this.view.archived === true, label: this.view.label, assignee: this.view.assignee,
        priority: this.view.priority.map((name) => String(priorityValue(name) ?? name)), signal,
      };
      const issues = continuing ? [...(previous?.issues ?? [])] : [];
      let cursor = continuing ? previous?.nextCursor : undefined;
      let pageCount = continuing ? previous!.pageCount : 0;
      let work: Loaded["work"];
      const cards: NativeBoardCard[] = continuing ? [...(previous?.cards ?? [])] : [];
      let sequence: number | undefined;
      do {
        signal.throwIfAborted();
        const page = await this.http.listWorkItems({ ...filters, limit: 100, ...tabQuery(this.view, toLanes(project)),
          sort: effectiveSort(this.view) === "closed" ? "closed" : undefined, cursor });
        issues.push(...page.items);
        cards.push(...(page.cards ?? []));
        work = page.work;
        sequence = Math.min(sequence ?? Infinity, page.sequence ?? 0);
        cursor = page.next_cursor;
        pageCount++;
      } while (cursor !== undefined && !continuing &&
        (pageCount < (previous?.pageCount ?? 1) || ["active", "backlog"].includes(this.view.tab)));
      return { project, issues, cards, sequence, work, nextCursor: cursor, pageCount,
        backlogPageCount: 0, backlogTotal: 0 };
    }
    const states = project.states.filter((state) => !state.terminal &&
      (this.view.lanes === null || this.view.lanes.includes(state.name)) &&
      (this.view.state.length === 0 || this.view.state.includes(state.name)));
    const backlog = states.find((state) => state.name.toLowerCase() === "backlog");
    const filters = this.streamFilters(id);
    const readPage = (state: string, cursor?: string) =>
      this.http.listWorkItems({ ...filters, signal, includeBoard: true, limit: state.toLowerCase() === "backlog" ? 200 : 2000, open: true, state, cursor });
    if (continuing) {
      if (previous?.backlogCursor === undefined || backlog === undefined) return previous!;
      const page = await readPage(backlog.name, previous.backlogCursor);
      return { ...previous, issues: [...previous.issues, ...page.items], cards: [...(previous.cards ?? []), ...(page.cards ?? [])],
        work: page.work ?? previous.work, backlogCursor: page.next_cursor, backlogTotal: page.total ?? previous.backlogTotal,
        backlogPageCount: previous.backlogPageCount + 1 };
    }
    const pages = await pooled(states, async (state) => {
      const issues: NativeIssue[] = [];
      const cards: NativeBoardCard[] = [];
      let cursor: string | undefined;
      let pageCount = 0;
      let total = 0;
      let sequence = Infinity;
      let work: Loaded["work"];
      do {
        const page = await readPage(state.name, cursor);
        issues.push(...page.items);
        cards.push(...(page.cards ?? []));
        cursor = page.next_cursor;
        sequence = Math.min(sequence, page.sequence ?? 0);
        work = page.work;
        total = page.total ?? total;
        pageCount++;
      } while (cursor !== undefined && pageCount < (state.name === backlog?.name ? previous?.backlogPageCount ?? 1 : 1));
      return { state: state.name, issues, cards, cursor, pageCount, total, sequence, work };
    });
    const back = pages.find((page) => page.state === backlog?.name);
    const fallback = pages.length === 0 ? await this.http.listWorkItems({ ...filters, signal, includeBoard: true, limit: 1 }) : undefined;
    return { project, issues: pages.flatMap((page) => page.issues), cards: pages.flatMap((page) => page.cards),
      work: pages[0]?.work ?? fallback?.work, sequence: pages.length === 0 ? fallback?.sequence ?? 0 : Math.min(...pages.map((page) => page.sequence)),
      pageCount: 1, backlogCursor: back?.cursor, backlogPageCount: back?.pageCount ?? 0, backlogTotal: back?.total ?? 0 };
  }

  streamFilters = (projectId: string) => ({ projectId, completedWindow: this.view.completedWindow, q: this.view.q,
    archived: this.view.archived === true, label: this.view.label, assignee: this.view.assignee,
    priority: this.view.priority.map((name) => String(priorityValue(name) ?? name)) });

  sequence = (projectId: string): number => this.entries.find((entry) => entry.project.project_id === projectId)?.sequence ?? 0;

  applyFrame = (projectId: string, frame: NativeBoardFrame, replay = false): void => {
    if (this.revoked || !this.scope.includes(projectId)) return;
    if (frame.gap) { this.reload(); return; }
    if (this.controller !== null && !replay) {
      this.frames.set(projectId, [...(this.frames.get(projectId) ?? []), frame]);
      return;
    }
    this.entries = this.entries.map((entry) => {
      if (entry.project.project_id !== projectId || frame.sequence < (entry.sequence ?? 0)) return entry;
      const issues = new Map(entry.issues.map((issue) => [issue.work_item_id, issue]));
      const cards = new Map((entry.cards ?? []).map((card) => [card.issue.work_item_id, card]));
      for (const delta of frame.deltas) {
        if (delta.sequence <= (entry.sequence ?? 0)) continue;
        const card = delta.current;
        const issue = card?.issue ?? delta.previous?.issue;
        if (issue === undefined || issue.project_id !== projectId) continue;
        if (newer(issues.get(issue.work_item_id)?.revision ?? null, issue.revision)) continue;
        issues.delete(issue.work_item_id);
        cards.delete(issue.work_item_id);
        if (card !== null && this.matches(card.issue, entry.project)) {
          issues.set(issue.work_item_id, card.issue);
          cards.set(issue.work_item_id, card);
        }
      }
      const work = frame.work ?? entry.work;
      return { ...entry, issues: [...issues.values()], cards: [...cards.values()], sequence: frame.sequence, work,
        backlogTotal: work?.lanes.find((lane) => lane.state.toLowerCase() === "backlog")?.total ?? entry.backlogTotal };
    });
    if (this.state.resolved) {
      this.publish({ ...this.build(this.entries, Date.now()), refreshing: this.state.refreshing, backlogLoading: this.state.backlogLoading });
      this.persist();
    }
  };

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
        for (const [id, frames] of this.frames) {
          for (const frame of frames) this.applyFrame(id, frame, true);
        }
        this.frames.clear();
        this.persist();
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
      let running = 0, waiting = 0, needAttention = 0, backlog = 0, closed = 0, total = 0;
      for (const entry of loaded) {
        for (const lane of entry.work!.lanes) {
          const state = entry.project.states.find((state) => state.name === lane.state);
          counts[lane.state] = (counts[lane.state] ?? 0) + lane.total;
          total += lane.total;
          if (state?.terminal) closed += lane.total;
          else {
            if (this.view.state.length === 0 || this.view.state.includes(lane.state)) running += lane.running;
            const idle = lane.total - lane.running;
            if (lane.state.toLowerCase() === "backlog") backlog += idle;
            else if (state?.dispatchable) waiting += idle;
            else needAttention += idle;
          }
        }
      }
      if (this.view.view === "board") {
        for (const entry of loaded) {
          const backlogState = entry.project.states.find((state) => state.name.toLowerCase() === "backlog");
          if (backlogState === undefined || entry.work!.lanes.some((lane) => lane.state === backlogState.name)) continue;
          counts[backlogState.name] = (counts[backlogState.name] ?? 0) + entry.backlogTotal;
          total += entry.backlogTotal;
          backlog += entry.backlogTotal;
        }
      }
      const completed = loaded.every((entry) => entry.work!.completed !== undefined)
        ? loaded.reduce((count, entry) => count + entry.work!.completed!, 0)
        : this.view.completedWindow === "all" ? closed : null;
      totals = { lanes: counts, running, waiting, needAttention, backlog, open: total - closed, completed, total,
        asOf: loaded.map((entry) => entry.work!.as_of).toSorted()[0]!,
        truncated: loaded.some((entry) => entry.work!.truncated) };
    }
    const issues = [...new Map(loaded.flatMap((entry) => entry.issues).map((issue) => [issue.work_item_id, issue])).values()];
    const items = issues.map((issue) => {
      const entry = loaded.find((entry) => entry.project.project_id === issue.project_id)!;
      const card = entry.cards?.find((card) => card.issue.work_item_id === issue.work_item_id);
      const previous = this.views.get(issue.work_item_id);
      if (previous?.issue === issue && previous.card === card) return previous.item;
      const item = card === undefined ? toWorkItemView(issue, entry.project.name) : toBoardCardView({ ...card, issue }, entry.project.name);
      const result = { ...item, terminal: entry.project.states.find((state) => state.name === issue.state)?.terminal ?? issue.terminal };
      this.views.set(issue.work_item_id, { issue, card, item: result });
      return result;
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
