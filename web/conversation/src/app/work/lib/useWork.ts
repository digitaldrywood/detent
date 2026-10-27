import React from "react";
import { subscribeProjectEvents } from "./projectEvents.ts";

import { useClient } from "../../client.ts";
import { useRunnerNames, shortRunnerId } from "./runnerNames.ts";
import { toWorkItemView } from "./fromWire.ts";
import { boardAccountKey, confirmWorkItem, getBoardRead, rejectBoardCache, type BoardData } from "./boardStore.ts";
import type { WorkItemView } from "./model.ts";
import { makeWorkHttp, newWorkKey, type WorkHttp, WorkApiError } from "./workHttp.ts";
import type { WorkViewState } from "./viewState.ts";

const clients = new WeakMap<object, WorkHttp>();

export function useWorkHttp(): WorkHttp {
  return workHttpFor(useClient());
}

/** The one work client for a conversation client, outside React. */
export function workHttpFor(client: ReturnType<typeof useClient>): WorkHttp {
  let http = clients.get(client);
  if (http === undefined) {
    const owner = boardAccountKey(client);
    http = makeWorkHttp({ origin: client.http.origin, apiBase: client.http.apiBase, csrfToken: client.http.csrfToken,
      onAuthorizationRejected: () => rejectBoardCache(owner), onConfirmed: (issue) => confirmWorkItem(issue, owner) });
    clients.set(client, http);
  }
  return http;
}

export interface BoardState extends BoardData {
  readonly sequence: number | null;
  readonly live: boolean;
}

export function useBoard(projectId: string | null, view: WorkViewState): BoardState & {
  readonly reload: () => void;
  readonly loadMore: () => void;
  readonly loadBacklog: () => void;
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
  const namedItems = React.useRef(new WeakMap<WorkItemView, { display: string; item: WorkItemView }>());
  const items = React.useMemo(() => {
    const names = new Map([...runnerNames].map(([id, name]) => [shortRunnerId(id), name.display]));
    return data.items.map((item) => {
      const display = item.attempt?.runner === null || item.attempt?.runner === undefined ? undefined : names.get(item.attempt.runner);
      if (display === undefined) return item;
      const previous = namedItems.current.get(item);
      if (previous?.display === display) return previous.item;
      const result = { ...item, attempt: { ...item.attempt!, runner: display } };
      namedItems.current.set(item, { display, item: result });
      return result;
    });
  }, [data.items, runnerNames]);
  React.useEffect(() => {
    setState({ key: streamKey, live: false, sequence: null });
    const connected = new Set<string>();
    const publish = () => setState((current) => ({ ...current, key: streamKey,
      live: connected.size === streamScope.length && streamScope.length > 0 }));
    const subscriptions = streamScope.map((id) => subscribeProjectEvents(http, id, {
      open: () => { connected.add(id); publish(); },
      error: () => { connected.delete(id); publish(); },
      reset: () => { connected.delete(id); publish(); },
      checkpoint: (event) => {
        const next = Number((event as MessageEvent).lastEventId);
        if (Number.isFinite(next)) setState((current) => ({ ...current, sequence: next }));
      },
    }));
    return () => subscriptions.forEach((stop) => stop());
  }, [http, store, streamKey, streamScope]);

  return { ...data, live: data.resolved && state.key === streamKey && state.live,
    sequence: state.key === streamKey ? state.sequence : null,
    items, reload, loadMore: store.loadMore, loadBacklog: store.loadBacklog, applyItem: store.applyItem };
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
