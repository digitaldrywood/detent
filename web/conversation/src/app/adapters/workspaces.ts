import React from "react";
import * as Schema from "effect/Schema";

import {
  Workspace,
  type WorkspaceReason,
  type WorkspaceState,
  WORKSPACE_EXISTS,
  WORKSPACE_LIMIT,
  WORKSPACE_UNUSABLE_STATES,
} from "../../contracts/work.ts";
import { newWorkKey, WorkApiError, type WorkHttp } from "../work/lib/workHttp.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";

/** The eight `workspace.<state>` event names §18.1 puts on the stream. */
export const WORKSPACE_EVENT_TYPES: readonly string[] = [
  "workspace.requested",
  "workspace.starting",
  "workspace.ready",
  "workspace.idle",
  "workspace.unreachable",
  "workspace.closing",
  "workspace.closed",
  "workspace.failed",
];

/** A workspace a surface can actually talk to. */
export function isWorkspaceUsable(state: WorkspaceState): boolean {
  return !WORKSPACE_UNUSABLE_STATES.has(state);
}

/** A workspace that is up and answering relay frames. */
export function isWorkspaceLive(state: WorkspaceState): boolean {
  return state === "ready" || state === "idle";
}

export interface WorkspaceHandle {
  readonly workspace: Workspace | null;
  /** Null until the first read or create answers. */
  readonly state: WorkspaceState | null;
  readonly reason: WorkspaceReason | null;
  /** A request that failed outright, as a sentence. Not a workspace `reason`. */
  readonly error: string | null;
  readonly loading: boolean;
  readonly retry: () => void;
  /** Opens a socket for this workspace's relay, ticket and all (§18.2). */
  readonly relayUrl: ((ticket: string) => string) | null;
  readonly mintTicket: (() => Promise<string>) | null;
}

const IDLE: WorkspaceHandle = {
  workspace: null,
  state: null,
  reason: null,
  error: null,
  loading: false,
  retry: () => {},
  relayUrl: null,
  mintTicket: null,
};

const KEY_STORAGE_PREFIX = "detent:workspace-key:v1";

/**
 * The idempotency key for "open a workspace for this issue", persisted.
 *
 * §18.1 answers a second request on the same worktree with 409
 * `workspace_exists`, and the create is a mutation, so a reload that retried
 * with a fresh key would be a different intent and could race its own earlier
 * attempt. One key per project-and-issue, kept across reloads, makes every
 * retry of that one intent the same request — which is what an idempotency key
 * is for.
 */
function workspaceKey(projectId: string, workItemId: string): string {
  const storageKey = `${KEY_STORAGE_PREFIX}:${projectId}:${workItemId}`;
  try {
    const existing = globalThis.localStorage?.getItem(storageKey);
    if (existing !== null && existing !== undefined && existing.length > 0) return existing;
  } catch {
    // Storage denied: a fresh key per session still works, it just cannot
    // deduplicate a create across a reload.
  }
  const minted = newWorkKey("workspace");
  try {
    globalThis.localStorage?.setItem(storageKey, minted);
  } catch {
    // As above.
  }
  return minted;
}

const decodeWorkspace = Schema.decodeUnknownSync(Workspace);

/** The workspace an event frame carries, or null when it is not one of ours. */
export function workspaceFromEvent(data: string, workspaceId: string): Workspace | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }
  let workspace: Workspace;
  try {
    workspace = decodeWorkspace(parsed);
  } catch {
    // A frame this build cannot decode is dropped rather than fatal; the next
    // transition re-states the whole resource anyway.
    return null;
  }
  // Its own workspace only. The stream is project-wide, and another reader's
  // workspace transitioning is none of this surface's business.
  return workspace.id === workspaceId ? workspace : null;
}

/**
 * Whether this workspace can serve what the caller asked for.
 *
 * The client-side half of the rule the hub enforces at claim time
 * (`Capabilities.Satisfies` in `internal/workspacesession/session.go`), and it
 * mirrors that function rather than inventing its own semantics: every
 * requested capability must be one the runner that claimed this workspace
 * actually reported.
 *
 * §18.12 writes down why it is needed and why it was latent until now. While
 * every workspace was a files workspace the question could not come up. With
 * two capabilities it can: a workspace already open with `requires: ["files"]`
 * would be reused for a caller asking for `["files", "exec"]`, the exec frames
 * would then be refused by the hub's channel gate, and the reader would see an
 * action that never produces output with nothing saying why.
 *
 * An absent or null `capabilities` is "not yet known", not "none": a workspace
 * in `requested` has not been claimed, so no runner has reported anything. It
 * is therefore reusable only for a caller that requires nothing — otherwise
 * adopting it is a bet on what a runner will turn out to report.
 */
export function workspaceSatisfies(
  workspace: Workspace,
  requires: readonly string[],
): boolean {
  const wanted = requires.filter((entry) => entry.length > 0);
  if (wanted.length === 0) return true;
  const capabilities = workspace.capabilities ?? null;
  if (capabilities === null) return false;
  const reported = capabilities as unknown as Record<string, unknown>;
  return wanted.every((entry) => reported[entry] === true);
}

/**
 * Finds an open workspace for this issue, or opens one.
 *
 * Reuse first, because §18.1 caps open workspaces per organization and per
 * person and answers a duplicate with 409: a reader who opens Files, closes
 * the panel and opens it again must land on the workspace they already have,
 * not spend another slot. Reuse is only correct where the workspace can serve
 * the surface, which is what `workspaceSatisfies` above decides.
 */
async function acquire(
  http: WorkHttp,
  projectId: string,
  workItemId: string,
  requires: readonly string[],
): Promise<Workspace> {
  const existing = await http
    .listWorkspaces({ projectId, workItemId })
    .then((page) =>
      page.workspaces.find(
        (candidate) =>
          isWorkspaceUsable(candidate.state) && workspaceSatisfies(candidate, requires),
      ),
    )
    .catch(() => undefined);
  if (existing !== undefined) return existing;
  try {
    return await http.createWorkspace({
      projectId,
      key: workspaceKey(projectId, workItemId),
      workItemId,
      requires,
    });
  } catch (cause) {
    if (!(cause instanceof WorkApiError)) throw cause;
    // 409 names the workspace that already exists; adopting it is the whole
    // point of the code, so this is a success path with an extra read — but
    // only where the workspace it names can serve this surface. Adopting one
    // that cannot is the same bug as reusing one that cannot, by a different
    // route, so the same check applies and the 409 is re-thrown otherwise:
    // the reader is told the workspace exists rather than handed one whose
    // frames will be refused.
    if (cause.status === 409 && cause.code === WORKSPACE_EXISTS) {
      const id = cause.details?.["workspace_id"];
      if (typeof id === "string") {
        const adopted = await http.getWorkspace(projectId, id);
        if (workspaceSatisfies(adopted, requires)) return adopted;
      }
    }
    throw cause;
  }
}

/** 422 `workspace_limit` says which cap was hit; a reader should be told. */
function describe(cause: unknown): string {
  if (!(cause instanceof WorkApiError)) {
    return cause instanceof Error ? cause.message : String(cause);
  }
  if (cause.code !== WORKSPACE_LIMIT) return cause.message;
  const scope = cause.details?.["scope"];
  const limit = cause.details?.["limit"];
  const who = scope === "person" ? "You have" : "This organization has";
  return typeof limit === "number"
    ? `${who} ${limit} workspaces open, which is the limit. Close one and try again.`
    : `${who} as many workspaces open as the plan allows. Close one and try again.`;
}

export interface UseWorkspaceInput {
  readonly projectId: string | null;
  readonly workItemId: string | null;
  /** What the runner must report to be allowed to claim it (§18.1). */
  readonly requires: readonly string[];
  /** False keeps the hook idle, so opening a panel is what spends a slot. */
  readonly enabled?: boolean;
}

export function useWorkspace(input: UseWorkspaceInput): WorkspaceHandle {
  const http = useWorkHttp();
  const { projectId, workItemId } = input;
  const enabled = input.enabled ?? true;
  // The array identity changes on every render at most call sites; the key is
  // what the effect actually depends on.
  const requiresKey = [...input.requires].sort().join(",");

  const [workspace, setWorkspace] = React.useState<Workspace | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [nonce, setNonce] = React.useState(0);
  const retry = React.useCallback(() => setNonce((value) => value + 1), []);

  React.useEffect(() => {
    if (!enabled || projectId === null || workItemId === null) {
      setWorkspace(null);
      setError(null);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    void acquire(http, projectId, workItemId, requiresKey.split(","))
      .then((next) => {
        if (cancelled) return;
        setWorkspace(next);
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        setWorkspace(null);
        setError(describe(cause));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [http, projectId, workItemId, requiresKey, enabled, nonce]);

  // Readiness by subscription (§18.1). The stream is the project's, so every
  // frame is filtered down to this workspace's id before it is believed.
  const workspaceId = workspace?.id ?? null;
  React.useEffect(() => {
    if (projectId === null || workspaceId === null) return;
    if (typeof globalThis.EventSource !== "function") return;
    const source = new globalThis.EventSource(http.eventsUrl(projectId), {
      withCredentials: true,
    });
    const onWorkspaceEvent = (event: MessageEvent<string>) => {
      const next = workspaceFromEvent(event.data, workspaceId);
      if (next === null) return;
      setWorkspace(next);
    };
    for (const type of WORKSPACE_EVENT_TYPES) {
      source.addEventListener(type, onWorkspaceEvent as EventListener);
    }
    return () => {
      for (const type of WORKSPACE_EVENT_TYPES) {
        source.removeEventListener(type, onWorkspaceEvent as EventListener);
      }
      source.close();
    };
  }, [http, projectId, workspaceId]);

  const relayUrl = React.useMemo(
    () =>
      projectId === null || workspaceId === null
        ? null
        : (ticket: string) => http.relayUrl(projectId, workspaceId, ticket),
    [http, projectId, workspaceId],
  );
  const mintTicket = React.useMemo(
    () =>
      projectId === null || workspaceId === null
        ? null
        : async () => {
            // A new key per ticket: each ticket is a separate single-use
            // intent (§18.2), so replaying one would be wrong, not idempotent.
            const minted = await http.mintRelayTicket({
              projectId,
              workspaceId,
              key: newWorkKey("relay"),
            });
            return minted.ticket;
          },
    [http, projectId, workspaceId],
  );

  return React.useMemo<WorkspaceHandle>(() => {
    if (!enabled || projectId === null || workItemId === null) return IDLE;
    return {
      workspace,
      state: workspace?.state ?? null,
      reason: workspace?.reason ?? null,
      error,
      loading,
      retry,
      relayUrl,
      mintTicket,
    };
  }, [enabled, projectId, workItemId, workspace, error, loading, retry, relayUrl, mintTicket]);
}

// The sentences a reader sees for a workspace that is not serving a surface —
// including the one for a workspace that failed or closed — live in
// `app/lib/workspaceStatus.ts`, because the Files surface and the Output
// surface both wait on the same session and two copies of nine sentences
// drift. Re-exported here so a caller that already holds this module's
// workspace hook does not have to know that.
export {
  describeWorkspaceStatus,
  workspaceReasonSentence,
  workspaceSessionFacts,
  type WorkspaceSessionFacts,
  type WorkspaceStatusPresentation,
} from "../lib/workspaceStatus.ts";
