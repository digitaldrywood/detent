import React from "react";

import { useClient } from "../../client.ts";
import { useRunnerNames, shortRunnerId } from "./runnerNames.ts";
import { toWorkItemView } from "./fromWire.ts";
import { boardAccountKey, confirmWorkItem, getBoardRead, rejectBoardCache, type BoardData } from "./boardStore.ts";
import type { WorkItemView } from "./model.ts";
import { makeWorkHttp, newWorkKey, type WorkHttp, WorkApiError } from "./workHttp.ts";
import type { WorkViewState } from "./viewState.ts";

const clients = new WeakMap<object, WorkHttp>();

export function useWorkHttp(): WorkHttp {
  const client = useClient();
  let http = clients.get(client);
  if (http === undefined) {
    const owner = boardAccountKey(client);
    http = makeWorkHttp({ origin: client.http.origin, apiBase: client.http.apiBase, csrfToken: client.http.csrfToken,
      onAuthorizationRejected: () => rejectBoardCache(owner), onConfirmed: (issue) => confirmWorkItem(issue, owner) });
    clients.set(client, http);
  }
  return http;
}

const EVENT_SOURCE_CLOSED = 2;
const STREAM_RETRY_MS = 3_000;

export interface BoardState extends BoardData {
  readonly sequence: number | null;
  readonly live: boolean;
}

export function useBoard(projectId: string | null, view: WorkViewState): BoardState & {
  readonly reload: () => void;
  readonly loadMore: () => void;
  readonly applyItem: (item: WorkItemView) => void;
} {
  const client = useClient();
  const http = useWorkHttp();
  const runnerNames = useRunnerNames();
  const store = getBoardRead(client, http, projectId, view);
  const streamScope = store.scope;
  const streamKey = JSON.stringify([store.owner, streamScope]);
  const data = React.useSyncExternalStore(store.subscribe, store.snapshot, store.snapshot);
  const [state, setState] = React.useState({ key: streamKey, live: false, sequence: null as number | null });
  const reload = store.reload;
  React.useEffect(() => { store.start(); }, [store]);
  const items = React.useMemo(() => {
    const names = new Map([...runnerNames].map(([id, name]) => [shortRunnerId(id), name.display]));
    return data.items.map((item) => {
      const display = item.attempt?.runner === null || item.attempt?.runner === undefined ? undefined : names.get(item.attempt.runner);
      return display === undefined ? item : { ...item, attempt: { ...item.attempt!, runner: display } };
    });
  }, [data.items, runnerNames]);
  const sequences = React.useRef({ key: streamKey, values: new Map<string, number>() });
  if (sequences.current.key !== streamKey) sequences.current = { key: streamKey, values: new Map() };
  React.useEffect(() => {
    if (streamScope.length === 0 || typeof globalThis.EventSource !== "function") {
      setState({ key: streamKey, live: false, sequence: null });
      return;
    }
    setState({ key: streamKey, live: false, sequence: null });
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
        return current.live === live && current.key === streamKey ? current : { ...current, key: streamKey, live };
      });

    const onActivity = (id: string, event: MessageEvent<string>) => {
      const next = Number.parseInt(event.data, 10);
      if (!Number.isFinite(next)) return;
      // A frame arriving is proof the stream is up whether or not `open`
      // fired: a reconnecting `EventSource` does not always announce itself.
      connected.add(id);
      const previous = sequences.current.values.get(id);
      sequences.current.values.set(id, next);
      setState((current) => ({
        ...current,
        key: streamKey,
        sequence: next,
        live: connected.size === streamScope.length,
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

  return { ...data, live: data.resolved && state.key === streamKey && state.live,
    sequence: state.key === streamKey ? state.sequence : null,
    items, reload, loadMore: store.loadMore, applyItem: store.applyItem };
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
