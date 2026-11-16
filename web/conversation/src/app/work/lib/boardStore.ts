import * as Schema from "effect/Schema";
import { LiveEntrySchema, LoadedSchema, type Loaded } from "./boardSchema.ts";
import { LiveBoard, liveCompleted, type LiveEntry } from "./liveBoard.ts";
import { subscribeProjectQuery } from "./projectEvents.ts";

import { NativeIssue, NativeProject, priorityValue, NativeBoardCard, NativeBoardFrame, type NativeShippedWindow, type NativeWorkSummary } from "../../../contracts/work.ts";
import { authorizeQueryCache, clearQueryCache, invalidateQueries } from "../../lib/queryCache.ts";
import { BOARD_CACHE_LIMIT, BOARD_CACHE_VERSION, readBoardDisk, updateBoardDisk } from "./boardDisk.ts";
import { toBoardCardView, toLanes, toProjectView, toWorkItemView } from "./fromWire.ts";
import type { Lane, ProjectView, ScopedWorkStats, ShippedCount, WorkItemView } from "./model.ts";
import { WorkApiError, type BoardCount, type WorkHttp } from "./workHttp.ts";
import { effectiveSort, tabMatches, tabQuery, type WorkViewState } from "./viewState.ts";

export interface BoardData {
  readonly resolved: boolean;
  readonly refreshing: boolean;
  readonly cached: boolean;
  readonly totals: ScopedWorkStats | null;
  readonly hasMore: boolean;
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

const REVOKED_MESSAGE = "Access to the board is no longer available.";
const LIVE_LIMIT = 4;
const BOARD_SEARCH_PAGES = 10;

const EMPTY: BoardData = {
  resolved: false, refreshing: false, cached: false, totals: null, hasMore: false, backlogTotal: 0,
  loading: true, error: null, project: null, workflows: [], lanes: [], items: [],
  labels: [], assignees: [], priorities: [], truncated: false, enriched: 0, asOf: null,
};


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

function rejectUnauthorized(owner: string, cause: unknown): boolean {
  if (!(typeof cause === "object" && cause !== null && "authorizationConfirmed" in cause && cause.authorizationConfirmed === true)) return false;
  for (const projectId of cause instanceof WorkApiError ? cause.authorizationProjects ?? [undefined] : [undefined]) {
    rejectBoardCache(owner, projectId);
  }
  return true;
}


interface LaneState {
  readonly name: string;
  readonly terminal: boolean;
}

export function liveBoardView(view: Pick<WorkViewState, "view" | "q" | "completedFrom" | "completedTo">): boolean {
  return view.view === "board" && view.q.trim().length === 0 && !view.completedFrom && !view.completedTo;
}

export interface BoardAccount {
  readonly http: { readonly origin: string; readonly apiBase: string };
  readonly bootstrap: {
    readonly organization: { readonly id: string };
    readonly actor?: { readonly principal_id: string };
    readonly projects: readonly { readonly id: string; readonly can_write?: boolean; readonly states?: readonly { readonly name: string; readonly terminal?: boolean }[] }[];
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
const lives = new Map<string, LiveBoard>();
let pinned: string | null = null;
const metadata = new Map<string, { at: number; read: Promise<NativeProject> }>();
const knownStates = new Map<string, readonly LaneState[]>();
const confirmed = new Map<string, NativeIssue>();
const facets = new Map<string, Pick<ProjectView, "labels" | "assignees" | "priorities">>();

export function authorizeBoardCache(next: string): void {
  authorizeQueryCache(next);
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
  knownStates.clear();
  confirmed.clear();
  facets.clear();
  for (const board of boards.values()) board.revoke();
  boards.clear();
  for (const live of lives.values()) live.revoke();
  lives.clear();
  pinned = null;
}

export function clearBoardCache(): void {
  clearQueryCache();
  clearMemory();
  const current = epoch;
  void updateBoardDisk(null, undefined, () => epoch === current);
}

export function rejectBoardCache(owner: string | null = account, projectId?: string): void {
  if (owner !== account) return;
  epoch++;
  metadata.clear();
  knownStates.clear();
  confirmed.clear();
  facets.clear();
  for (const board of boards.values()) {
    if (projectId === undefined || board.scope.includes(projectId)) board.revoke();
  }
  for (const [key, live] of lives) {
    live.revoke(projectId);
    if (live.state.revoked.size === live.scope.length) lives.delete(key);
  }
  const current = epoch;
  void updateBoardDisk(null, undefined, () => epoch === current);
}

function getProject(http: WorkHttp, id: string): Promise<NativeProject> {
  const previous = metadata.get(id);
  if (previous !== undefined && Date.now() - previous.at < 60_000) return previous.read;
  const read = http.getProject(id).then((project) => {
    knownStates.set(id, project.states);
    return project;
  }, (cause) => {
    if (metadata.get(id)?.read === read) metadata.delete(id);
    throw cause;
  });
  metadata.set(id, { at: Date.now(), read });
  return read;
}

function normalized(view: WorkViewState): WorkViewState {
  const values = (entries: readonly string[]) => [...new Set(entries)].sort();
  return { ...view, q: view.q.trim(), state: view.view === "list" && !view.completedFrom && !view.completedTo ? [] : values(view.state), type: view.view === "list" ? values(view.type) : [], label: values(view.label),
    assignee: values(view.assignee), priority: values(view.priority), lanes: view.lanes === null ? null : values(view.lanes) };
}

function boardScope(client: BoardAccount, projectId: string | null, projectIds?: readonly string[]): string[] {
  const accessible = client.bootstrap.projects.map((project) => project.id).sort();
  return projectIds === undefined
    ? (projectId === null ? accessible : accessible.includes(projectId) ? [projectId] : [])
    : [...new Set(projectIds)].filter((id) => accessible.includes(id)).sort();
}

function newer(a: string | null, b: string | null): boolean {
  if (a === null || b === null) return false;
  try { return BigInt(a) > BigInt(b); } catch { return false; }
}

function overlay(card: NativeBoardCard): NativeBoardCard {
  const mutation = confirmed.get(card.issue.work_item_id);
  return mutation !== undefined && newer(mutation.revision, card.issue.revision) ? { ...card, issue: mutation } : card;
}

const decodeLive = Schema.decodeUnknownSync(Schema.Array(LiveEntrySchema));

function getLiveBoard(owner: string, http: WorkHttp, scope: readonly string[], archived: boolean): LiveBoard {
  const key = JSON.stringify([owner, scope, archived]);
  let live = lives.get(key) ?? [...lives.values()].find((candidate) => candidate.archived === archived && candidate.http === http &&
    scope.every((id) => candidate.scope.includes(id) && !candidate.state.revoked.has(id)));
  if (live !== undefined) {
    lives.delete(live.key);
    lives.set(live.key, live);
    return live;
  }
  const current = epoch;
  live = new LiveBoard(key, http, scope, archived, {
    overlay, newer,
    unauthorized: (cause) => rejectUnauthorized(owner, cause),
    states: (id, project) => { knownStates.set(id, project.states); },
    restore: async () => {
      const mutations = mutationVersion;
      const stored = await readBoardDisk(key);
      if (stored === null || epoch !== current || mutations !== mutationVersion || stored.account !== owner || stored.version !== BOARD_CACHE_VERSION) return null;
      try { return decodeLive(stored.entries); } catch { return null; }
    },
    persist: (entries, asOf) => {
      void updateBoardDisk(owner, { key, account: owner, version: BOARD_CACHE_VERSION, asOf, entries }, () => epoch === current && lives.get(key) === live);
    },
  });
  lives.set(key, live);
  for (const [oldKey, old] of lives) {
    if (lives.size <= LIVE_LIMIT) break;
    if (old === live || oldKey === pinned || old.listening) continue;
    old.dispose();
    lives.delete(oldKey);
    for (const [boardKey, board] of boards) if (board.live === old) boards.delete(boardKey);
  }
  return live;
}

export function retainLiveBoard(client: BoardAccount, http: WorkHttp, projectIds?: readonly string[]): () => void {
  const owner = boardAccountKey(client);
  authorizeBoardCache(owner);
  const scope = boardScope(client, null, projectIds !== undefined && projectIds.length > 0 ? projectIds : undefined);
  if (scope.length === 0) return () => {};
  const live = getLiveBoard(owner, http, scope, false);
  pinned = live.key;
  live.start();
  return () => { if (pinned === live.key) pinned = null; };
}

export function getBoardRead(client: BoardAccount, http: WorkHttp, projectId: string | null, input: WorkViewState, projectIds?: readonly string[], aggregate = false): BoardRead {
  const owner = boardAccountKey(client);
  authorizeBoardCache(owner);
  const scope = boardScope(client, projectId, projectIds);
  const view = normalized(input);
  const key = JSON.stringify([owner, projectId, scope, aggregate, view.q, view.state, view.type, view.label, view.assignee,
    view.priority.map((name) => String(priorityValue(name) ?? name)).sort(), view.archived === true, view.completedWindow, view.completedFrom, view.completedTo,
    view.createdBy, view.view, view.view === "list" ? view.tab : view.lanes, effectiveSort(view) === "closed"]);
  let board = boards.get(key);
  if (board !== undefined) {
    boards.delete(key);
    boards.set(key, board);
    return board;
  }
  board = new BoardRead(owner, key, http, projectId, scope, view, client.bootstrap.actor?.principal_id,
    liveBoardView(view) && scope.length > 0 ? getLiveBoard(owner, http, scope, view.archived === true) : null);
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
  for (const live of lives.values()) live.confirm(issue);
  for (const board of boards.values()) board.confirm(issue);
}

export class BoardRead {
  readonly subscribers = new Set<() => void>();
  private state: BoardData = EMPTY;
  private entries: readonly Loaded[] = [];
  private controller: AbortController | null = null;
  private readonly countControllers = new Map<string, AbortController>();
  private countsPending = new Set<string>();
  private failed = new Map<string, string>();
  private readonly frames = new Map<string, NativeBoardFrame[]>();
  private readonly views = new Map<string, { issue: NativeIssue; card: NativeBoardCard | undefined; item: WorkItemView }>();
  private generation = 0;
  private pending = false;
  private readonly queries = new Map<string, ReturnType<typeof subscribeProjectQuery>>();
  private readonly dirtyProjects = new Set<string>();
  private snapshotProjects = new Set<string>();
  private continuation = false;
  private revoked = false;
  private readonly facetKey: string;
  private seen = -1;
  private unlisten: (() => void) | null = null;
  private readonly derived = new WeakMap<LiveEntry, Loaded>();

  constructor(readonly owner: string, readonly key: string, readonly http: WorkHttp,
    readonly projectId: string | null, readonly scope: readonly string[], readonly view: WorkViewState,
    private readonly principalId?: string, readonly live: LiveBoard | null = null) {
    this.facetKey = JSON.stringify([owner, scope, view.archived === true]);
    if (scope.length === 0) this.state = { ...EMPTY, loading: false, resolved: true };
    else {
      const previous = [...boards.values()].find((board) => board.facetKey === this.facetKey && board.state.resolved);
      if (previous !== undefined) this.state = { ...EMPTY, lanes: previous.state.lanes,
        workflows: previous.state.workflows, project: previous.state.project, ...facets.get(this.facetKey) };
    }
  }

  projectSnapshot(projectId: string | null): BoardData {
    this.sync();
    if (projectId === null || !this.state.resolved || this.entries.length === 0) return this.state;
    const entries = this.entries.filter((entry) => entry.project.project_id === projectId);
    if (entries.length === 0) return { ...EMPTY, loading: this.state.loading, resolved: this.state.resolved, error: this.state.error };
    const data = this.build(entries, this.state.asOf ?? Date.now());
    return { ...data, project: toProjectView(entries[0]!.project, entries[0]!.issues),
      cached: this.state.cached, loading: this.state.loading,
      refreshing: this.state.refreshing, error: this.state.error };
  }

  projectCounts(): Readonly<Record<string, { running: number; blocked: number }>> {
    this.sync();
    return Object.fromEntries(this.entries.map((entry) => {
      const runners = new Set(entry.cards?.flatMap((card) => {
        const runner = card.execution?.running ? card.execution.runner_id : card.attempt?.status === "running" ? card.attempt.runner_id : undefined;
        return runner === undefined ? [] : [runner];
      }) ?? []);
      return [entry.project.project_id, {
        running: runners.size || entry.work?.lanes.reduce((count, lane) => count + lane.running, 0) || 0,
        blocked: entry.work?.lanes.filter((lane) => lane.state.toLowerCase() === "blocked").reduce((count, lane) => count + lane.total, 0)
          ?? entry.issues.filter((issue) => issue.state.toLowerCase() === "blocked" && !issue.terminal).length,
      }];
    }));
  }

  snapshot = (): BoardData => {
    this.sync();
    return this.state;
  };

  subscribe = (notify: () => void): (() => void) => {
    this.subscribers.add(notify);
    if (this.subscribers.size === 1) {
      if (this.live !== null) this.unlisten = this.live.subscribe(() => { if (this.sync()) this.notify(); });
      else this.listen();
    }
    return () => {
      this.subscribers.delete(notify);
      if (this.subscribers.size > 0) return;
      this.unlisten?.();
      this.unlisten = null;
      for (const query of this.queries.values()) query.close();
      this.queries.clear();
      this.dispose();
    };
  };

  private listen(): void {
    for (const id of this.scope) {
      const query = subscribeProjectQuery(this.http, id, {
        topics: ["work_item", "board", "lane", "activity"],
        apply: (event) => {
          if (event.type === "activity") {
            try {
              const frame = Schema.decodeUnknownSync(NativeBoardFrame)(JSON.parse((event as MessageEvent<string>).data));
              this.applyFrame(id, frame);
            } catch { return; }
          } else {
            if (event.type === "lane") metadata.delete(id);
            query.refresh();
          }
        },
        restart: () => {
          const targets = new Set([...this.snapshotProjects, ...this.dirtyProjects, ...this.countControllers.keys(), id]);
          this.dispose();
          metadata.delete(id);
          this.read(false, targets);
        },
        refresh: () => this.refresh(id),
      });
      this.queries.set(id, query);
    }
  }

  private notify(): void {
    for (const notify of this.subscribers) notify();
  }

  private publish(state: BoardData): void {
    this.state = state;
    this.notify();
  }

  dispose(): void {
    if (this.live !== null) return;
    this.generation++;
    this.controller?.abort();
    for (const countController of this.countControllers.values()) countController.abort();
    this.countControllers.clear();
    this.controller = null;
    this.pending = false;
  }

  revoke(): void {
    this.revoked = true;
    this.dispose();
    this.entries = [];
    this.failed.clear();
    this.frames.clear();
    this.views.clear();
    this.state = { ...EMPTY, loading: false, error: REVOKED_MESSAGE };
    queueMicrotask(() => this.notify());
  }

  private sync(): boolean {
    const live = this.live;
    if (live === null || this.revoked) return false;
    const current = live.state;
    if (current.version === this.seen) return false;
    this.seen = current.version;
    if (this.scope.some((id) => current.revoked.has(id))) {
      const changed = this.state.error !== REVOKED_MESSAGE || this.entries.length > 0;
      this.entries = [];
      this.views.clear();
      if (changed) this.state = { ...EMPTY, loading: false, error: REVOKED_MESSAGE };
      return changed;
    }
    const entries = this.scope.flatMap((id) => {
      const entry = current.entries.get(id);
      return entry === undefined ? [] : [this.derive(entry)];
    });
    const failed = new Map(this.scope.flatMap((id) => {
      const reason = current.failed.get(id);
      return reason === undefined ? [] : [[id, `${this.scope.length > 1 ? `${current.entries.get(id)?.project.name ?? id}: ` : ""}${reason}`] as const];
    }));
    const countsPending = new Set(this.scope.filter((id) => current.countsPending.has(id)));
    const unchanged = entries.length === this.entries.length && entries.every((entry, index) => entry === this.entries[index]) &&
      sameEntries(failed, this.failed) && countsPending.size === this.countsPending.size && [...countsPending].every((id) => this.countsPending.has(id));
    this.failed = failed;
    this.countsPending = countsPending;
    const error = [current.error, this.errors(entries)].filter((value) => value !== null).join("; ") || null;
    const status = { resolved: current.resolved, refreshing: current.refreshing, cached: current.cached, loading: !current.resolved && error === null, error };
    if (unchanged) {
      if (this.state.resolved === status.resolved && this.state.refreshing === status.refreshing && this.state.cached === status.cached &&
        this.state.loading === status.loading && this.state.error === status.error) return false;
      this.state = { ...this.state, ...status };
      return true;
    }
    this.entries = entries;
    const data = entries.length === 0 ? { ...EMPTY, lanes: this.state.lanes } : this.build(entries, current.asOf ?? Date.now());
    this.state = { ...data, ...status };
    return true;
  }

  private derive(entry: LiveEntry): Loaded {
    const existing = this.derived.get(entry);
    if (existing !== undefined) return existing;
    const cards = entry.cards.filter((card) => this.matches(card.issue, entry.project));
    const work = this.localCounts() ? localWork(cards, entry.work) : entry.work === undefined ? undefined : { ...entry.work, ...liveCompleted(entry.work, this.view.completedWindow) };
    const loaded: Loaded = { project: entry.project, issues: cards.map((card) => card.issue), cards, sequence: entry.sequence, grantVersion: entry.grantVersion, work,
      pageCount: 1, backlogTotal: work?.lanes.find((lane) => lane.state.toLowerCase() === "backlog")?.total ?? 0 };
    this.derived.set(entry, loaded);
    return loaded;
  }

  private localCounts(): boolean {
    return this.view.type.length > 0 || this.view.label.length > 0 || this.view.assignee.length > 0 || this.view.priority.length > 0 || this.view.createdBy !== undefined;
  }

  private persist(): void {
    if (this.live !== null || this.entries.length !== this.scope.length || this.failed.size > 0 || this.state.asOf === null) return;
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
    if (continuing && previous?.nextCursor === undefined) return previous!;
    const board = this.view.view === "board";
    const filters = {
      projectId: id, includeBoard: true, completedWindow: this.view.completedWindow, completedFrom: this.view.completedFrom, completedTo: this.view.completedTo, q: this.view.q,
      createdBy: this.view.createdBy, archived: this.view.archived === true, type: this.view.type, label: this.view.label, assignee: this.view.assignee,
      priority: this.view.priority.map((name) => String(priorityValue(name) ?? name)), signal,
    };
    const issues = continuing ? [...(previous?.issues ?? [])] : [];
    let cursor = continuing ? previous?.nextCursor : undefined;
    let pageCount = continuing ? previous!.pageCount : 0;
    let work: Loaded["work"];
    const cards: NativeBoardCard[] = continuing ? [...(previous?.cards ?? [])] : [];
    let sequence: number | undefined = continuing ? previous?.sequence : undefined;
    let grantVersion = continuing ? previous?.grantVersion : undefined;
    do {
      signal.throwIfAborted();
      const page = await this.http.listWorkItems({ ...filters, cursor, ...(board ? { limit: 2000, open: true }
        : { limit: 100, ...tabQuery(this.view, toLanes(project)), sort: effectiveSort(this.view) === "closed" ? "closed" : undefined }) });
      if (page.projection_pending) throw new Error("Board projection is pending");
      issues.push(...page.items);
      cards.push(...(page.cards ?? []));
      work = page.total === -1 ? previous?.work : page.work;
      sequence = Math.min(sequence ?? Infinity, page.change_sequence ?? page.sequence ?? 0);
      grantVersion ??= page.grant_version;
      cursor = page.next_cursor;
      pageCount++;
    } while (cursor !== undefined && !continuing && (board ? pageCount < BOARD_SEARCH_PAGES
      : pageCount < (previous?.pageCount ?? 1) || ["active", "backlog"].includes(this.view.tab)));
    const backlogTotal = board ? work?.lanes.find((lane) => lane.state.toLowerCase() === "backlog")?.total ?? 0 : 0;
    return { project, issues, cards, sequence, grantVersion, work, nextCursor: board ? undefined : cursor, pageCount, backlogTotal };
  }

  streamFilters = (projectId: string) => ({ projectId, completedWindow: this.view.completedWindow, completedFrom: this.view.completedFrom, completedTo: this.view.completedTo, q: this.view.q,
    createdBy: this.view.createdBy, archived: this.view.archived === true, type: this.view.type, label: this.view.label, assignee: this.view.assignee,
    priority: this.view.priority.map((name) => String(priorityValue(name) ?? name)) });

  sequence = (projectId: string): number => {
    this.sync();
    return this.entries.find((entry) => entry.project.project_id === projectId)?.sequence ?? 0;
  };

  applyFrame = (projectId: string, frame: NativeBoardFrame, replay = false): void => {
    if (this.live !== null || this.revoked || !this.scope.includes(projectId)) return;
    if (frame.gap || this.entries.some((entry) => entry.project.project_id === projectId &&
      entry.grantVersion !== undefined && frame.grant_version !== undefined && entry.grantVersion !== frame.grant_version)) {
      (this.queries.get(projectId)?.refresh ?? this.reload)(); return;
    }
    if (this.controller !== null && !replay) {
      this.frames.set(projectId, [...(this.frames.get(projectId) ?? []), frame]);
      return;
    }
    if (this.view.q.length > 0 && frame.sequence > (this.entries.find((entry) => entry.project.project_id === projectId)?.sequence ?? 0)) {
      (this.queries.get(projectId)?.refresh ?? this.reload)();
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
        for (const id of new Set([card?.issue.work_item_id, delta.previous?.issue.work_item_id])) {
          if (id !== undefined) invalidateQueries(["issue", id]);
        }
        if (issue === undefined || issue.project_id !== projectId) continue;
        if (newer(issues.get(issue.work_item_id)?.revision ?? null, issue.revision)) continue;
        issues.delete(issue.work_item_id);
        cards.delete(issue.work_item_id);
        if (card !== null && this.matches(card.issue, entry.project)) {
          issues.set(issue.work_item_id, card.issue);
          cards.set(issue.work_item_id, card);
        }
      }
      const work = frame.counts_independent ? entry.work : frame.work ?? entry.work;
      return { ...entry, issues: [...issues.values()], cards: [...cards.values()], sequence: frame.sequence, work,
        backlogTotal: work?.lanes.find((lane) => lane.state.toLowerCase() === "backlog")?.total ?? entry.backlogTotal };
    });
    if (this.state.resolved) {
      this.publish({ ...this.build(this.entries, this.stamp()), refreshing: this.state.refreshing });
      this.persist();
    }
    if (!replay && (this.failed.has(projectId) || this.countsPending.has(projectId))) this.queries.get(projectId)?.refresh();
  };

  private matches(issue: NativeIssue, project: NativeProject): boolean {
    const view = this.view;
    return (!view.completedFrom || typeof issue.closed_at === "string" && Date.parse(issue.closed_at) >= Date.parse(view.completedFrom)) &&
      (!view.completedTo || typeof issue.closed_at === "string" && Date.parse(issue.closed_at) < Date.parse(view.completedTo)) &&
      (!(view.completedFrom || view.completedTo) || view.state.length === 0 || view.state.includes(issue.state)) &&
      (view.createdBy !== "me" || issue.actor.principal_id === this.principalId) && (view.view !== "board" || (!issue.terminal && project.states.some((state) => state.name === issue.state && !state.terminal) &&
      (view.lanes === null || view.lanes.includes(issue.state)))) && (issue.archived === true) === (view.archived === true) &&
      (view.view === "list" ? tabMatches(view.tab, { state: issue.state, terminal: project.states.find((state) => state.name === issue.state)?.terminal ?? issue.terminal })
        : view.state.length === 0 || view.state.includes(issue.state)) &&
      (view.type.length === 0 || issue.type !== undefined && view.type.includes(issue.type)) &&
      (view.label.length === 0 || view.label.some((label) => issue.labels.includes(label))) &&
      (view.assignee.length === 0 || view.assignee.some((assignee) => issue.assignees.includes(assignee))) &&
      (view.priority.length === 0 || view.priority.some((name) => String(priorityValue(name) ?? name) === String(issue.priority)));
  }

  reload = (): void => {
    if (this.live !== null) {
      this.live.start();
      this.live.reload();
      return;
    }
    if (this.controller !== null) { this.pending = true; return; }
    this.revoked = false;
    this.read(false);
  };

  private refresh(projectId: string): void {
    this.dirtyProjects.add(projectId);
    if (this.controller !== null) { this.pending = true; return; }
    this.read(false, new Set([projectId]));
  }

  start = (): void => {
    if (this.live !== null) this.live.start();
    else if (this.controller === null) this.reload();
  };

  loadMore = (): void => {
    if (this.live !== null || this.view.view === "board" || !this.state.hasMore || this.continuation) return;
    this.dispose();
    this.read(true);
  };

  private read(continuing: boolean, targets?: ReadonlySet<string>): void {
    if (this.revoked || this.scope.length === 0 || this.live !== null) return;
    const controller = new AbortController();
    this.controller = controller;
    this.continuation = continuing;
    const signal = controller.signal;
    const generation = ++this.generation;
    let prior = this.entries;
    this.snapshotProjects = new Set(targets ?? this.scope);
    const snapshots = new Map([...this.queries].filter(([id]) => targets === undefined || targets.has(id))
      .map(([id, query]) => [id, query.beginSnapshot()]));
    this.dirtyProjects.clear();
    this.publish({ ...this.state, loading: !this.state.resolved || continuing, refreshing: true, error: null });
    if (!this.state.resolved) void this.restore(generation);
    const counts = new Map<string, readonly BoardCount[]>();
    let cardsReady = false;
    const withCounts = (entry: Loaded): Loaded => {
      const rows = counts.get(entry.project.project_id);
      if (rows === undefined) return entry;
      const total = rows.find((row) => row.kind === "total" || row.kind === undefined && row.state === "*");
      const windows = ["1h", "24h", "7d"] as const;
      const lanes = rows.filter((row) => row.kind === "lane" || row.kind === undefined && !["*", "@open", "@closed"].includes(row.state)).map(({ state, total, running }) => ({ state, total, running }));
      return { ...entry, backlogTotal: lanes.find((row) => row.state.toLowerCase() === "backlog")?.total ?? 0,
        work: { items: entry.work?.items ?? [], lanes, truncated: false,
          completed: total?.[`shipped_${this.view.completedWindow}`] ?? 0,
          cancelled: total?.[`cancelled_${this.view.completedWindow}`] ?? 0,
          shipped_windows: windows.map((window) => ({ window, shipped: total?.[`shipped_${window}`] ?? 0, cancelled: total?.[`cancelled_${window}`] ?? 0 })),
          as_of: total?.projected_at ?? rows[0]?.projected_at ?? entry.work?.as_of ?? "0001-01-01T00:00:00Z" } };
    };
    const supportsCounts = this.view.q.length === 0 && this.view.type.length === 0 && this.view.label.length === 0 &&
      this.view.assignee.length === 0 && this.view.priority.length === 0 && this.view.createdBy === undefined &&
      !this.view.completedFrom && !this.view.completedTo;
    const countControllers = new Map((supportsCounts ? this.scope.filter((id) => targets === undefined || targets.has(id)) : []).map((id) => {
      this.countControllers.get(id)?.abort();
      const countController = new AbortController();
      this.countControllers.set(id, countController);
      return [id, countController] as const;
    }));
    const countsRead = pooled([...countControllers], async ([id, countController]) => {
      const current = () => this.countControllers.get(id) === countController && !this.revoked;
      try {
        const rows = await this.http.getBoardCounts({ projectId: id, archived: this.view.archived === true, signal: countController.signal });
        if (!current()) return;
        this.countsPending.delete(id);
        counts.set(id, rows);
        if (!cardsReady) return;
        this.entries = this.entries.map((entry) => entry.project.project_id === id ? withCounts(entry) : entry);
        this.publish({ ...this.build(this.entries, this.state.asOf ?? Date.now()), refreshing: this.state.refreshing });
        this.persist();
      } catch (cause) {
        if (rejectUnauthorized(this.owner, cause)) return;
        if (!current()) return;
        if (typeof cause === "object" && cause !== null && "code" in cause && cause.code === "not_ready") {
          this.countsPending.add(id);
          this.entries = this.entries.map((entry) => entry.project.project_id === id ? { ...entry, work: undefined } : entry);
          if (cardsReady) this.publish(this.build(this.entries, this.state.asOf ?? Date.now()));
        }
      } finally {
        if (this.countControllers.get(id) === countController) this.countControllers.delete(id);
      }
    });
    void (async () => {
      try {
        const failures = new Map<string, { label: string; cause: unknown }>();
        const succeeded = new Set<string>();
        const loaded = await pooled(this.scope, async (id): Promise<Loaded | undefined> => {
          if (!continuing && prior.length === 0 && this.entries.length > 0) prior = this.entries;
          const previous = prior.find((entry) => entry.project.project_id === id);
          if (targets !== undefined && !targets.has(id) && (previous !== undefined || this.failed.has(id))) return previous;
          let entry: Loaded;
          try {
            entry = await this.loadProject(id, signal, previous, continuing);
          } catch (cause) {
            if (rejectUnauthorized(this.owner, cause) || signal.aborted) throw cause;
            failures.set(id, { label: previous?.project.name ?? id, cause });
            return previous;
          }
          succeeded.add(id);
          const issues = entry.issues;
          let changed = false;
          const previousItems = new Map(this.entries.flatMap((entry) => entry.issues).map((issue) => [issue.work_item_id, issue]));
          const current = [...new Map(issues.map((issue) => [issue.work_item_id, issue])).values()]
            .map((issue) => {
              if (this.view.q.length > 0) return issue;
              const mutation = confirmed.get(issue.work_item_id);
              const previous = previousItems.get(issue.work_item_id);
              const latest = newer(previous?.revision ?? null, mutation?.revision ?? null) ? previous : mutation ?? previous;
              if (latest === undefined || !newer(latest.revision, issue.revision)) return issue;
              changed = true;
              return latest;
            }).filter((issue) => this.matches(issue, entry.project));
          const ids = new Set(current.map((issue) => issue.work_item_id));
          for (const issue of confirmed.values()) {
            if (this.view.q.length > 0) break;
            if (issue.project_id !== id || ids.has(issue.work_item_id) || !this.matches(issue, entry.project)) continue;
            current.push(issue);
            changed = true;
          }
          return { ...entry, issues: current, work: changed ? undefined : entry.work };
        });
        if (signal.aborted || generation !== this.generation || [...snapshots.values()].some((snapshot) => !snapshot.current())) return;
        const entries = loaded.map((entry, index) => {
          const id = this.scope[index]!;
          return targets === undefined || targets.has(id) ? entry : this.entries.find((current) => current.project.project_id === id) ?? entry;
        }).filter((entry) => entry !== undefined);
        for (const id of succeeded) this.failed.delete(id);
        for (const [id, { label, cause }] of failures) {
          this.failed.set(id, `${this.scope.length > 1 ? `${label}: ` : ""}${cause instanceof Error ? cause.message : String(cause)}`);
        }
        if (entries.length === 0) throw [...failures.values()][0]!.cause;
        if (succeeded.size === 0) {
          for (const snapshot of snapshots.values()) snapshot.fail();
          this.publish({ ...this.state, loading: false, refreshing: false, error: this.errors(this.entries) });
          return;
        }
        this.entries = entries.map(withCounts);
        cardsReady = true;
        this.publish(this.build(this.entries, this.stamp()));
        for (const [id, frames] of this.frames) {
          for (const frame of frames) this.applyFrame(id, frame, true);
        }
        this.frames.clear();
        this.persist();
        this.controller = null;
        void countsRead.then(() => {
          for (const [id, snapshot] of snapshots) {
            if (failures.has(id) || this.countsPending.has(id)) snapshot.fail();
            else snapshot.complete(this.sequence(id));
          }
        });
      } catch (cause) {
        if (signal.aborted || generation !== this.generation) return;
        for (const snapshot of snapshots.values()) snapshot.fail();
        if (!rejectUnauthorized(this.owner, cause)) this.publish({ ...this.state, loading: false, refreshing: false,
          error: cause instanceof Error ? cause.message : String(cause) });
      } finally {
        if (generation === this.generation) {
          this.controller = null;
          this.snapshotProjects.clear();
          this.continuation = false;
          if (this.pending) {
            this.pending = false;
            this.read(false, this.dirtyProjects.size > 0 ? new Set(this.dirtyProjects) : undefined);
          }
        }
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
    const counted = loaded.filter((entry) => entry.work !== undefined && !this.countsPending.has(entry.project.project_id));
    if (counted.length > 0) {
      const counts = Object.create(null) as Record<string, number>;
      let running = 0, waiting = 0, needAttention = 0, backlog = 0, closed = 0, total = 0;
      for (const entry of counted) {
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
      const completed = counted.every((entry) => entry.work!.completed !== undefined)
        ? counted.reduce((count, entry) => count + entry.work!.completed!, 0)
        : null;
      const cancelled = counted.every((entry) => entry.work!.cancelled !== undefined)
        ? counted.reduce((count, entry) => count + entry.work!.cancelled!, 0)
        : null;
      totals = { lanes: counts, running, waiting, needAttention, backlog, open: total - closed, completed, cancelled,
        shippedWindows: shippedWindows(counted.map((entry) => entry.work!.shipped_windows)), total,
        asOf: counted.map((entry) => entry.work!.as_of).toSorted()[0]!,
        truncated: counted.some((entry) => entry.work!.truncated) };
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
    return { resolved: true, refreshing: false, cached: false, loading: false, error: this.errors(loaded),
      totals, hasMore: this.view.view !== "board" && loaded.some((entry) => entry.nextCursor !== undefined),
      backlogTotal: loaded.reduce((total, entry) => total + entry.backlogTotal, 0),
      project: this.projectId === null ? null : { ...current, ...choices, lanes },
      workflows: loaded.map((entry) => entry.project), lanes, items, ...choices,
      truncated: loaded.some((entry) => entry.nextCursor !== undefined), enriched: 0, asOf };
  }

  private stamp(): number {
    return this.failed.size > 0 ? this.state.asOf ?? Date.now() : Date.now();
  }

  private errors(loaded: readonly Loaded[]): string | null {
    const label = (id: string) => loaded.find((entry) => entry.project.project_id === id)?.project.name ?? id;
    const counts = this.countsPending.size === 0 ? [] : [`Board counts projection is pending${this.scope.length > 1 ? `: ${[...this.countsPending].map(label).join(", ")}` : ""}`];
    const errors = [...this.failed.values(), ...counts];
    return errors.length > 0 ? errors.join("; ") : null;
  }

  confirm(issue: NativeIssue): void {
    if (this.live !== null || !this.scope.includes(issue.project_id)) return;
    if (this.view.q.length > 0) {
      this.refresh(issue.project_id);
      return;
    }
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
    this.refresh(issue.project_id);
  }

  applyItem = (item: WorkItemView): void => {
    this.sync();
    const issue = this.entries.flatMap((entry) => entry.issues).find((candidate) => candidate.work_item_id === item.id);
    if (issue === undefined || item.revision === null) return;
    confirmWorkItem({ ...issue, state: item.state, revision: item.revision, terminal: item.terminal,
      archived: item.archived, title: item.title, body: item.body, labels: item.labels, assignees: item.assignees });
  };
}

function sameEntries(a: ReadonlyMap<string, string>, b: ReadonlyMap<string, string>): boolean {
  return a.size === b.size && [...a].every(([key, value]) => b.get(key) === value);
}

function localWork(cards: readonly NativeBoardCard[], base: NativeWorkSummary | undefined): NativeWorkSummary {
  const lanes = new Map<string, { state: string; total: number; running: number }>();
  for (const card of cards) {
    const lane = lanes.get(card.issue.state) ?? { state: card.issue.state, total: 0, running: 0 };
    lanes.set(card.issue.state, { ...lane, total: lane.total + 1, running: lane.running + (card.execution?.running || card.attempt?.status === "running" ? 1 : 0) });
  }
  return { items: [], lanes: [...lanes.values()], truncated: false, as_of: base?.as_of ?? new Date(0).toISOString() };
}

function shippedWindows(summaries: readonly (readonly NativeShippedWindow[] | undefined)[]): Readonly<Record<string, ShippedCount>> | null {
  if (summaries.some((windows) => windows === undefined)) return null;
  const totals: Record<string, ShippedCount> = {};
  for (const windows of summaries) {
    for (const window of windows!) {
      const current = totals[window.window];
      totals[window.window] = { shipped: (current?.shipped ?? 0) + window.shipped, cancelled: (current?.cancelled ?? 0) + window.cancelled };
    }
  }
  return totals;
}
