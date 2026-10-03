// The board's data.
//
// Reads, not state management: the hub has no board projection, so this hook
// is where the several calls that add up to one are made, bounded, and turned
// into the view model. Everything it does that is not a plain fetch is here
// because the API made it necessary, and each one says which gap it is
// working around.
import React from "react";

import type { BootstrapProject } from "../../../contracts/index.ts";
import { priorityValue, type NativeAttempt, type NativeIssue, type NativeProject, type NativeWorkSummary } from "../../../contracts/work.ts";
import { useClient } from "../../client.ts";
import { useRunnerNames } from "./runnerNames.ts";
import { toChangeView, toProjectView, toWorkItemView } from "./fromWire.ts";
import type { Lane, ProjectView, WorkItemView, ScopedWorkStats } from "./model.ts";
import { makeWorkHttp, newWorkKey, type WorkHttp, WorkApiError } from "./workHttp.ts";
import type { WorkViewState } from "./viewState.ts";

/** One `WorkHttp` per client. A second one would only duplicate the config. */
export function useWorkHttp(): WorkHttp {
  const client = useClient();
  return React.useMemo(
    () =>
      makeWorkHttp({
        origin: client.http.origin,
        apiBase: client.http.apiBase,
        csrfToken: client.http.csrfToken,
      }),
    [client],
  );
}

/**
 * How many issues one load reads per project, and how many of them get the
 * per-item follow-up calls.
 *
 * The hub serves no "what is running" projection on a work item and no
 * project-scoped changes list, so the worker strip and the PR chip each cost
 * one request per issue. That is an N+1 the board must not run unbounded over
 * a 500-issue backlog, so it runs over the most recently updated non-terminal
 * issues only, and the rest render without a strip rather than with a wrong
 * one.
 */
const PAGE_LIMIT = 100;
const ENRICH_LIMIT = 24;
const ENRICH_CONCURRENCY = 6;

/**
 * `EventSource.CLOSED`, spelled out rather than read off the constructor: a
 * test double supplies a `readyState` but not the class constants.
 */
const EVENT_SOURCE_CLOSED = 2;

/** How long a closed activity stream waits before it is opened again. */
const STREAM_RETRY_MS = 3_000;

/** Runs `work` over `inputs`, at most `limit` at a time. */
async function pooled<A, B>(
  inputs: readonly A[],
  limit: number,
  work: (input: A) => Promise<B>,
): Promise<B[]> {
  const results: B[] = new Array(inputs.length);
  let next = 0;
  const workers = Array.from({ length: Math.min(limit, inputs.length) }, async () => {
    for (;;) {
      const index = next;
      next += 1;
      if (index >= inputs.length) return;
      results[index] = await work(inputs[index]!);
    }
  });
  await Promise.all(workers);
  return results;
}

export interface BoardState {
  readonly totals: ScopedWorkStats | null;
  readonly hasMore: boolean;
  readonly loading: boolean;
  readonly error: string | null;
  /** Null in the all-projects scope, where lanes are the union of every one. */
  readonly project: ProjectView | null;
  readonly lanes: readonly Lane[];
  readonly items: readonly WorkItemView[];
  readonly labels: readonly string[];
  readonly assignees: readonly string[];
  readonly priorities: readonly string[];
  readonly truncated: boolean;
  readonly enriched: number;
  readonly asOf: number | null;
  /** The activity sequence from the hosted stream, or null when not streaming. */
  readonly sequence: number | null;
  readonly live: boolean;
}

interface Loaded {
  readonly work?: NativeWorkSummary;
  readonly project: NativeProject;
  readonly issues: readonly NativeIssue[];
  readonly nextCursor?: string;
  readonly pageCount: number;
}

async function loadProject(
  http: WorkHttp,
  projectId: string,
  view: WorkViewState,
  signal: AbortSignal,
  cursor?: string,
): Promise<Loaded> {
  const project = await http.getProject(projectId, signal);
  const page = await http.listWorkItems({
    projectId,
    limit: PAGE_LIMIT,
    includeWork: true,
    q: view.q,
    state: view.state,
    archived: view.archived === true,
    label: view.label,
    assignee: view.assignee,
    priority: view.priority.map((name) => String(priorityValue(name) ?? name)),
    cursor,
    signal,
  });
  return {
    project,
    issues: [...(page.work?.items ?? []), ...page.items],
    work: page.work,
    nextCursor: page.next_cursor,
    pageCount: 1,
  };
}

/**
 * Loads a board.
 *
 * `projectId` is `null` for the all-projects scope, which reads every project
 * the bootstrap payload lists — there is no list-projects endpoint, so the
 * bootstrap is the only directory this client has.
 */
export function useBoard(
  projectId: string | null,
  view: WorkViewState,
): BoardState & {
  readonly reload: () => void;
  readonly loadMore: () => void;
  /**
   * Replaces one item with what the hub just answered, so a successful
   * mutation lands on the board at once instead of waiting for the activity
   * stream's tick and the reload behind it (optimistic board).
   */
  readonly applyItem: (item: WorkItemView) => void;
} {
  const client = useClient();
  const http = useWorkHttp();
  const runnerNames = useRunnerNames();
  const latestRunnerNames = React.useRef(runnerNames);
  latestRunnerNames.current = runnerNames;
  const projects: readonly BootstrapProject[] = client.bootstrap.projects;
  const [state, setState] = React.useState<BoardState & { requestKey: string; requestHttp: WorkHttp }>({
    requestKey: "",
    requestHttp: http,
    totals: null,
    hasMore: false,
    loading: true,
    error: null,
    project: null,
    lanes: [],
    items: [],
    labels: [],
    assignees: [],
    priorities: [],
    truncated: false,
    enriched: 0,
    asOf: null,
    sequence: null,
    live: false,
  });
  const [nonce, setNonce] = React.useState(0);
  const activeRead = React.useRef<AbortController | null>(null);
  const refreshPending = React.useRef(false);
  const [continuation, setContinuation] = React.useState<{ key: string; cursors: Record<string, string> }>({ key: "", cursors: {} });
  const reload = React.useCallback(() => {
    if (activeRead.current !== null) {
      refreshPending.current = true;
      return;
    }
    setNonce((value) => value + 1);
  }, []);
  const applyItem = React.useCallback((item: WorkItemView) => {
    setState((current) => ({
      ...current,
      items: current.items.map((candidate) => candidate.id === item.id
        ? { ...item, attempt: candidate.attempt, change: candidate.change, observations: candidate.observations }
        : candidate),
    }));
  }, []);

  const scope = projectId === null ? projects.map((project) => project.id) : [projectId];
  const facetScope = JSON.stringify([scope, view.archived === true]);
  const knownFacets = React.useRef<{ scope: string; http: WorkHttp; facets: Pick<ProjectView, "labels" | "assignees" | "priorities"> }>({ scope: "", http, facets: { labels: [], assignees: [], priorities: [] } });
  const filterKey = JSON.stringify([scope, view.q.trim(), view.state, view.label, view.assignee, view.priority, view.archived === true]);
  const selectionKey = filterKey;
  const cursors = continuation.key === selectionKey ? continuation.cursors : {};
  const requestKey = JSON.stringify([selectionKey, nonce, cursors]);
  const loadedSelection = React.useRef<{ key: string; nonce: number; http: WorkHttp; entries: Loaded[]; cursors: Record<string, string> }>({ key: "", nonce: -1, http, entries: [], cursors: {} });
  const loadMore = React.useCallback(() => {
    if (state.loading || loadedSelection.current.key !== selectionKey || loadedSelection.current.http !== http) return;
    const next = { ...loadedSelection.current.cursors };
    for (const entry of loadedSelection.current.entries) {
      if (entry.nextCursor !== undefined) next[entry.project.project_id] = entry.nextCursor;
    }
    setContinuation({ key: selectionKey, cursors: next });
  }, [http, selectionKey, state.loading]);

  React.useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();
    const signal = controller.signal;
    activeRead.current = controller;
    if (scope.length === 0) {
      activeRead.current = null;
      setState((current) => ({ ...current, requestKey, requestHttp: http, hasMore: false, loading: false, error: null,
        totals: null, project: null, items: [], lanes: [], labels: [], assignees: [], priorities: [], truncated: false, enriched: 0, asOf: null }));
      return;
    }
    const cachedSelection = loadedSelection.current.key === selectionKey && loadedSelection.current.http === http ? loadedSelection.current : null;
    const continuing = cachedSelection !== null && scope.some((id) => cachedSelection.cursors[id] !== cursors[id]);
    const background = cachedSelection !== null && cachedSelection.nonce !== nonce && !continuing;
    setState((current) => ({ ...current, requestKey, requestHttp: http, loading: !background, error: null,
      ...(loadedSelection.current.key === selectionKey && loadedSelection.current.http === http ? {} : {
        hasMore: false, totals: null, items: [], lanes: [], project: null, labels: [], assignees: [], priorities: [], asOf: null, truncated: false, enriched: 0,
      }),
    }));

    void (async () => {
      try {
        const cached = loadedSelection.current.key === selectionKey && loadedSelection.current.http === http ? loadedSelection.current : null;
        const refreshing = cached !== null && cached.nonce !== nonce;
        const loaded = await pooled(scope, ENRICH_CONCURRENCY, async (id) => {
          signal.throwIfAborted();
          const prior = cached?.entries.find((entry) => entry.project.project_id === id);
          if (!refreshing && prior !== undefined && cached?.cursors[id] === cursors[id]) return prior;
          let entry = await loadProject(http, id, view, signal, refreshing ? undefined : cursors[id]);
          const issues = refreshing ? [...entry.issues] : [...(prior?.issues ?? []), ...entry.issues];
          let pageCount = refreshing ? 1 : (prior?.pageCount ?? 0) + 1;
          const requestedPages = (prior?.pageCount ?? 1) + (cached?.cursors[id] !== cursors[id] ? 1 : 0);
          while (refreshing && pageCount < requestedPages && entry.nextCursor !== undefined) {
            entry = await loadProject(http, id, view, signal, entry.nextCursor);
            issues.push(...entry.issues);
            pageCount++;
          }
          return { ...entry, pageCount, issues: [...new Map(issues
            .map((issue) => [issue.work_item_id, issue])).values()] };
        });
        if (cancelled) return;

        // Lane order is the workflow's array order. Across projects the first
        // project to name a lane fixes its position; a second project's own
        // order cannot reorder a lane that is already placed, or the board
        // would shuffle when a project finishes loading.
        const lanes: Lane[] = [];
        const seen = new Set<string>();
        for (const entry of loaded) {
          for (const lane of toProjectView(entry.project, entry.issues).lanes) {
            if (seen.has(lane.name)) continue;
            seen.add(lane.name);
            lanes.push(lane);
          }
        }

        const names = new Map(loaded.map((entry) => [entry.project.project_id, entry.project.name]));
        let totals: ScopedWorkStats | null = null;
        if (loaded.every((entry) => entry.work !== undefined)) {
          const counts = Object.create(null) as Record<string, number>;
          let running = 0, queued = 0, completed = 0, total = 0;
          for (const entry of loaded) {
            for (const lane of entry.work!.lanes) {
              const state = entry.project.states.find((state) => state.name === lane.state);
              counts[lane.state] = (counts[lane.state] ?? 0) + lane.total;
              total += lane.total;
              running += lane.running;
              if (state?.terminal) completed += lane.total;
              else if (state?.dispatchable) queued += lane.total - lane.running;
            }
          }
          totals = { lanes: counts, running, queued, open: total - completed, completed, total,
            asOf: loaded.map((entry) => entry.work!.as_of).toSorted()[0]!,
            truncated: loaded.some((entry) => entry.work!.truncated) };
        }
        const issues = [...new Map(loaded.flatMap((entry) => entry.issues)
          .map((issue) => [issue.work_item_id, issue])).values()];

        const operationalOrder = new Map(loaded.flatMap((entry) =>
          (entry.work?.items ?? []).map((issue, index) => [issue.work_item_id, index] as const)));
        const enrichable = issues
          .filter((issue) => !issue.terminal)
          .toSorted((a, b) => (operationalOrder.get(a.work_item_id) ?? Infinity) - (operationalOrder.get(b.work_item_id) ?? Infinity)
            || Date.parse(b.updated_at) - Date.parse(a.updated_at))
          .slice(0, ENRICH_LIMIT);

        const extras = new Map<string, {
          attempts: NativeAttempt[];
          change: WorkItemView["change"];
          observations: NonNullable<WorkItemView["observations"]>;
        }>();
        await pooled(enrichable, ENRICH_CONCURRENCY, async (issue) => {
          signal.throwIfAborted();
          const [attemptRead, changeRead] = await Promise.allSettled([
            http.listAttempts(issue.project_id, issue.work_item_id, 10, signal),
            http.listChanges(issue.project_id, issue.work_item_id, signal),
          ]);
          signal.throwIfAborted();
          for (const read of [attemptRead, changeRead]) {
            if (read.status === "rejected" && read.reason instanceof WorkApiError &&
                [401, 403].includes(read.reason.status)) throw read.reason;
          }
          const change = changeRead.status === "fulfilled" ? changeRead.value.at(-1) : undefined;
          let detail = null;
          if (change !== undefined) {
            try {
              detail = await http.getChange(issue.project_id, issue.work_item_id, change.change_id, signal);
            } catch (cause) {
              if (cause instanceof WorkApiError && [401, 403].includes(cause.status)) throw cause;
            }
          }
          signal.throwIfAborted();
          extras.set(issue.work_item_id, {
            attempts: attemptRead.status === "fulfilled" ? [...attemptRead.value.items] : [],
            change: change === undefined ? null : toChangeView(change, detail),
            observations: {
              worker: attemptRead.status === "rejected" ? "unavailable"
                : attemptRead.value.next_cursor === undefined ? "known" : "partial",
              change: changeRead.status === "rejected" ? "unavailable"
                : change !== undefined && detail === null ? "partial" : "known",
            },
          });
        });
        if (cancelled) return;

        const items = issues.map((issue) => {
          const extra = extras.get(issue.work_item_id);
          return toWorkItemView(issue, names.get(issue.project_id) ?? issue.project_id, {
            ...(extra === undefined ? {} : { attempts: extra.attempts, change: extra.change, observations: extra.observations }),
            runnerNames: latestRunnerNames.current,
          });
        });
        const currentFacets = toProjectView(loaded[0]!.project, issues);
        const previousFacets = knownFacets.current.scope === facetScope && knownFacets.current.http === http
          ? knownFacets.current.facets : { labels: [], assignees: [], priorities: [] };
        const facets = { ...currentFacets,
          labels: [...new Set([...previousFacets.labels, ...currentFacets.labels])].toSorted(),
          assignees: [...new Set([...previousFacets.assignees, ...currentFacets.assignees])].toSorted(),
          priorities: [...new Set([...previousFacets.priorities, ...currentFacets.priorities])].toSorted(),
        };
        knownFacets.current = { scope: facetScope, http, facets };

        // `sequence` and `live` belong to the stream, not to this read, and
        // are carried across untouched. Publishing `live: false` here — which
        // this used to do — dropped the chip to "Not streaming" on every
        // reload, including the reload the stream itself had just asked for.
        loadedSelection.current = { key: selectionKey, nonce, http, entries: loaded, cursors };
        setState((current) => ({
          requestKey,
          requestHttp: http,
          totals,
          hasMore: loaded.some((entry) => entry.nextCursor !== undefined),
          loading: false,
          error: null,
          project:
            projectId === null
              ? null
              : { ...facets, id: loaded[0]!.project.project_id, name: loaded[0]!.project.name, lanes },
          lanes,
          items,
          labels: facets.labels,
          assignees: facets.assignees,
          priorities: facets.priorities,
          truncated: loaded.some((entry) => entry.nextCursor !== undefined),
          enriched: enrichable.length,
          asOf: Date.now(),
          sequence: current.sequence,
          live: current.live,
        }));
      } catch (cause) {
        if (cancelled) return;
        controller.abort();
        knownFacets.current = { scope: "", http, facets: { labels: [], assignees: [], priorities: [] } };
        setState((current) => ({
          ...current,
          loading: false,
          hasMore: false,
          totals: null,
          project: null,
          items: [],
          lanes: [],
          labels: [],
          assignees: [],
          priorities: [],
          enriched: 0,
          asOf: null,
          error: cause instanceof Error ? cause.message : String(cause),
        }));
      } finally {
        if (!cancelled && activeRead.current === controller) {
          activeRead.current = null;
          if (refreshPending.current) {
            refreshPending.current = false;
            reload();
          }
        }
      }
    })();

    return () => {
      cancelled = true;
      controller.abort();
      if (activeRead.current === controller) {
        activeRead.current = null;
        refreshPending.current = false;
      }
    };
  }, [http, projects, nonce, requestKey, reload]);

  // The hosted activity stream. It carries one integer for the whole project
  // and no event id, so it cannot say what changed and cannot be resumed: the
  // only honest response to an increase is to read the board again.
  //
  // One stream per project the board is showing, the all-projects scope
  // included. Three things used to stop the chip reading "Live" while the
  // client itself was live (Michael's review, September 12):
  //
  //  1. `/work` opened no stream at all. The effect began `if (projectId ===
  //     null) return`, so the all-projects board — the default route — never
  //     subscribed to anything and the chip stayed on its initial `false`.
  //  2. The load published `live: false` (fixed above), which dropped the chip
  //     on every reload, including the one the stream had just asked for.
  //  3. A stream the browser had *closed* rather than merely lost was never
  //     reconnected, so one 403 or one proxy hang-up left the board dark until
  //     the reader pressed Reload.
  const sequences = React.useRef(new Map<string, number>());
  const streamScope = React.useMemo(
    () => (projectId === null ? projects.map((project) => project.id) : [projectId]),
    [projectId, projects],
  );
  // The effect's identity is the set of projects it subscribes to, not the
  // array's: `projects` is a fresh array on every bootstrap read.
  const streamKey = streamScope.join(" ");
  React.useEffect(() => {
    if (streamScope.length === 0 || typeof globalThis.EventSource !== "function") {
      setState((current) => (current.live ? { ...current, live: false } : current));
      return;
    }
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const connected = new Set<string>();
    const sources = new Map<string, EventSource>();
    const retries = new Map<string, ReturnType<typeof setTimeout>>();

    // Live means every project in scope is connected: on the all-projects
    // board one project whose stream is down is activity the reader is not
    // being told about, and a chip that said "Live" would be lying about it.
    const publish = () =>
      setState((current) => {
        const live = connected.size === streamScope.length;
        return current.live === live ? current : { ...current, live };
      });

    const onActivity = (id: string, event: MessageEvent<string>) => {
      const next = Number.parseInt(event.data, 10);
      if (!Number.isFinite(next)) return;
      // A frame arriving is proof the stream is up whether or not `open`
      // fired: a reconnecting `EventSource` does not always announce itself.
      connected.add(id);
      const previous = sequences.current.get(id);
      sequences.current.set(id, next);
      setState((current) => ({
        ...current,
        sequence: next,
        live: connected.size === streamScope.length,
        asOf: Date.now(),
      }));
      if (previous === undefined || next <= previous) return;
      // Coalesced: a burst of ticks is one reload, not one per tick.
      clearTimeout(timer);
      timer = setTimeout(reload, 400);
    };

    const connect = (id: string) => {
      if (stopped) return;
      const source = new globalThis.EventSource(http.eventsUrl(id), { withCredentials: true });
      sources.set(id, source);
      source.addEventListener("open", () => {
        connected.add(id);
        publish();
      });
      source.addEventListener("activity", ((event: MessageEvent<string>) =>
        onActivity(id, event)) as EventListener);
      source.addEventListener("error", () => {
        connected.delete(id);
        publish();
        // The browser retries a stream it has only lost. One it has closed it
        // never retries, so this is where a board that would otherwise stay
        // dark comes back on its own.
        if (source.readyState !== EVENT_SOURCE_CLOSED) return;
        source.close();
        sources.delete(id);
        clearTimeout(retries.get(id));
        retries.set(id, setTimeout(() => connect(id), STREAM_RETRY_MS));
      });
    };

    for (const id of streamScope) connect(id);

    return () => {
      stopped = true;
      clearTimeout(timer);
      for (const retry of retries.values()) clearTimeout(retry);
      for (const source of sources.values()) source.close();
    };
  }, [http, reload, streamKey, streamScope]);

  return {
    ...state,
    ...(state.requestHttp === http && state.requestKey === requestKey || loadedSelection.current.key === selectionKey && loadedSelection.current.http === http ? {} : {
      totals: null, loading: true, error: null, project: null, items: [], lanes: [], hasMore: false,
      labels: [], assignees: [], priorities: [], asOf: null, truncated: false, enriched: 0,
    }),
    ...(knownFacets.current.scope === facetScope && knownFacets.current.http === http ? knownFacets.current.facets : {}),
    reload,
    loadMore,
    applyItem,
  };
}

export type TransitionOutcome =
  | { readonly ok: true; readonly item: WorkItemView }
  | { readonly ok: false; readonly conflict: boolean; readonly message: string };

/**
 * Moves one item to a lane.
 *
 * The key is minted per intent so a retry of the same move is idempotent, and
 * the revision is the one the card was rendered from — which is what makes a
 * concurrent move a `409` instead of a silent overwrite. A hosted `409`
 * carries no `current_revision`, so the caller's job on a conflict is to
 * re-read, not to retry with a guessed number.
 */
export async function moveItem(
  http: WorkHttp,
  item: WorkItemView,
  toState: string,
  projectName: string,
): Promise<TransitionOutcome> {
  if (item.revision === null) {
    return { ok: false, conflict: false, message: "This issue has no revision to move from." };
  }
  try {
    const updated = await http.transition({
      projectId: item.projectId,
      itemId: item.id,
      key: newWorkKey("move"),
      expectedRevision: item.revision,
      state: toState,
    });
    return { ok: true, item: toWorkItemView(updated, projectName) };
  } catch (cause) {
    if (cause instanceof WorkApiError) {
      return { ok: false, conflict: cause.conflict, message: cause.message };
    }
    return {
      ok: false,
      conflict: false,
      message: cause instanceof Error ? cause.message : String(cause),
    };
  }
}

/** A ticking clock for the age and elapsed labels, at one update a second. */
let clockNow = Date.now();
let clockTimer: ReturnType<typeof setInterval> | null = null;
const clockListeners = new Set<() => void>();

function subscribeClock(listener: () => void): () => void {
  if (clockTimer === null) {
    clockNow = Date.now();
    clockTimer = setInterval(() => {
      clockNow = Date.now();
      for (const notify of clockListeners) notify();
    }, 1_000);
  }
  clockListeners.add(listener);
  return () => {
    clockListeners.delete(listener);
    if (clockListeners.size === 0 && clockTimer !== null) {
      clearInterval(clockTimer);
      clockTimer = null;
    }
  };
}

function clockSnapshot(): number {
  return clockNow;
}

export function useNow(): number {
  return React.useSyncExternalStore(subscribeClock, clockSnapshot, clockSnapshot);
}
