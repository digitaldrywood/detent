// The board's data.
//
// Reads, not state management: the hub has no board projection, so this hook
// is where the several calls that add up to one are made, bounded, and turned
// into the view model. Everything it does that is not a plain fetch is here
// because the API made it necessary, and each one says which gap it is
// working around.
import React from "react";

import type { BootstrapProject } from "../../../contracts/index.ts";
import type { NativeAttempt, NativeIssue, NativeProject } from "../../../contracts/work.ts";
import { useClient } from "../../client.ts";
import { toChangeView, toProjectView, toWorkItemView } from "./fromWire.ts";
import type { Lane, ProjectView, WorkItemView } from "./model.ts";
import { makeWorkHttp, newWorkKey, serverFilter, type WorkHttp, WorkApiError } from "./workHttp.ts";
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
  readonly loading: boolean;
  readonly error: string | null;
  /** Null in the all-projects scope, where lanes are the union of every one. */
  readonly project: ProjectView | null;
  readonly lanes: readonly Lane[];
  readonly items: readonly WorkItemView[];
  readonly labels: readonly string[];
  readonly assignees: readonly string[];
  readonly priorities: readonly string[];
  /** True when the hub returned a cursor this load did not follow. */
  readonly truncated: boolean;
  /** How many issues carry live attempt and change data. */
  readonly enriched: number;
  readonly asOf: number | null;
  /** The activity sequence from the hosted stream, or null when not streaming. */
  readonly sequence: number | null;
  readonly live: boolean;
}

interface Loaded {
  readonly project: NativeProject;
  readonly issues: readonly NativeIssue[];
  readonly truncated: boolean;
}

async function loadProject(
  http: WorkHttp,
  projectId: string,
  view: WorkViewState,
): Promise<Loaded> {
  const project = await http.getProject(projectId);
  const page = await http.listWorkItems({
    projectId,
    limit: PAGE_LIMIT,
    // Single-valued only: the hub rejects a repeated query parameter, so one
    // value goes to the server and any others are applied client-side.
    state: serverFilter(view.state),
    label: serverFilter(view.label),
    assignee: serverFilter(view.assignee),
    priority: undefined,
  });
  return {
    project,
    issues: page.items,
    truncated: page.next_cursor !== undefined,
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
  /**
   * Replaces one item with what the hub just answered, so a successful
   * mutation lands on the board at once instead of waiting for the activity
   * stream's tick and the reload behind it (optimistic board).
   */
  readonly applyItem: (item: WorkItemView) => void;
} {
  const client = useClient();
  const http = useWorkHttp();
  const projects: readonly BootstrapProject[] = client.bootstrap.projects;
  const [state, setState] = React.useState<BoardState>({
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
  const reload = React.useCallback(() => setNonce((value) => value + 1), []);
  const applyItem = React.useCallback((item: WorkItemView) => {
    setState((current) => ({
      ...current,
      items: current.items.map((candidate) => (candidate.id === item.id ? item : candidate)),
    }));
  }, []);

  // Only the server-side filters belong in the dependency list: search, sort,
  // lane visibility and the extra values of a multi-select are applied to what
  // is already loaded, and refetching for them would make the board flicker on
  // every keystroke.
  const serverState = serverFilter(view.state);
  const serverLabel = serverFilter(view.label);
  const serverAssignee = serverFilter(view.assignee);

  React.useEffect(() => {
    let cancelled = false;
    const scope = projectId === null ? projects.map((project) => project.id) : [projectId];
    if (scope.length === 0) {
      setState((current) => ({ ...current, loading: false, items: [], lanes: [] }));
      return;
    }
    setState((current) => ({ ...current, loading: true, error: null }));

    void (async () => {
      try {
        const loaded = await pooled(scope, ENRICH_CONCURRENCY, (id) =>
          loadProject(http, id, {
            ...view,
            state: serverState === undefined ? [] : [serverState],
            label: serverLabel === undefined ? [] : [serverLabel],
            assignee: serverAssignee === undefined ? [] : [serverAssignee],
          }),
        );
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
        const issues = loaded.flatMap((entry) => entry.issues);
        const terminal = new Set(lanes.filter((lane) => lane.terminal).map((lane) => lane.name));

        // The enrichment budget goes to the issues a reader is most likely to
        // be watching: not finished, most recently touched.
        const enrichable = issues
          .filter((issue) => !terminal.has(issue.state))
          .toSorted((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at))
          .slice(0, ENRICH_LIMIT);

        const extras = new Map<string, { attempts: NativeAttempt[]; change: WorkItemView["change"] }>();
        await pooled(enrichable, ENRICH_CONCURRENCY, async (issue) => {
          const [attempts, changes] = await Promise.all([
            http
              .listAttempts(issue.project_id, issue.work_item_id, 10)
              .then((page) => [...page.items])
              .catch(() => [] as NativeAttempt[]),
            http.listChanges(issue.project_id, issue.work_item_id).catch(() => []),
          ]);
          const change = changes.at(-1);
          // One more call, and only for an issue that actually has a change:
          // the summary and the external PR number live on the detail, not on
          // the list row.
          const detail =
            change === undefined
              ? null
              : await http
                  .getChange(issue.project_id, issue.work_item_id, change.change_id)
                  .catch(() => null);
          extras.set(issue.work_item_id, {
            attempts,
            change: change === undefined ? null : toChangeView(change, detail),
          });
        });
        if (cancelled) return;

        const items = issues.map((issue) => {
          const extra = extras.get(issue.work_item_id);
          return toWorkItemView(issue, names.get(issue.project_id) ?? issue.project_id, {
            ...(extra === undefined ? {} : { attempts: extra.attempts, change: extra.change }),
          });
        });
        const facets = toProjectView(
          loaded[0]!.project,
          issues,
        );

        // `sequence` and `live` belong to the stream, not to this read, and
        // are carried across untouched. Publishing `live: false` here — which
        // this used to do — dropped the chip to "Not streaming" on every
        // reload, including the reload the stream itself had just asked for.
        setState((current) => ({
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
          truncated: loaded.some((entry) => entry.truncated),
          enriched: enrichable.length,
          asOf: Date.now(),
          sequence: current.sequence,
          live: current.live,
        }));
      } catch (cause) {
        if (cancelled) return;
        setState((current) => ({
          ...current,
          loading: false,
          error: cause instanceof Error ? cause.message : String(cause),
        }));
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [http, projectId, projects, nonce, serverState, serverLabel, serverAssignee]);

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

  return { ...state, reload, applyItem };
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
export function useNow(intervalMs = 1_000): number {
  const [now, setNow] = React.useState(() => Date.now());
  React.useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs]);
  return now;
}
