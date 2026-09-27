import React from "react";

import type { AttemptDiff, ChangeDetail, NativeAttempt } from "../../contracts/work.ts";
import type { PullRequestState } from "../../contracts/ui.ts";
import {
  agentPanelModelFromAttempts,
  type AgentPanelModel,
  type AttemptActivity,
} from "./subagentRuntime.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import type { WorkHttp } from "../work/lib/workHttp.ts";

/** One changed file in a change request's current version. */
export interface ChangedFile {
  readonly path: string;
  readonly status: "added" | "modified" | "removed" | "renamed";
  readonly additions: number | null;
  readonly deletions: number | null;
  /** The unified patch for this file, when the hub serves one. */
  readonly patch: string | null;
}

export interface PullRequestSurfaceData {
  readonly number: number;
  readonly repository: string;
  readonly url: string;
  readonly state: PullRequestState;
  readonly isDraft: boolean;
  readonly title: string;
}

/**
 * Where the Diff surface's body comes from (decisions.md §18.5, §19).
 *
 * Two sources, in this order, because only one of them has files in it:
 *
 *  - `attempt` — a stored attempt diff. The runner posts its worktree diff
 *    before every `run.checkpointed` and before `run.finished`, with the file
 *    list, the per-file counts and the patches, so this is a diff a reader can
 *    actually read.
 *  - `version` — the change request's current version. Its code is one opaque
 *    artifact (`{kind, uri, sha256, availability}`) with no file names in it,
 *    so this is the round's identity and its verdict, not a diff. It is the
 *    fallback for an issue no attempt has posted a diff for.
 *  - `unavailable` — neither, with the reason.
 */
export type DiffSource =
  | { readonly kind: "attempt"; readonly diff: AttemptDiff }
  | { readonly kind: "version" }
  | { readonly kind: "unavailable"; readonly reason: string };

/**
 * Picks the Diff surface's source. Pure, so the order is asserted directly
 * rather than through a mounted panel: an attempt diff beats the version card
 * whatever the version says, and only an issue with neither is unavailable.
 */
export function diffSource(input: {
  readonly attemptDiff: AttemptDiff | null;
  readonly change: ChangeDetail | null;
  readonly codeAvailability: string | null;
  readonly linkedToIssue: boolean;
}): DiffSource {
  if (input.attemptDiff !== null) return { kind: "attempt", diff: input.attemptDiff };
  if (!input.linkedToIssue) {
    return { kind: "unavailable", reason: "This chat is not linked to an issue yet." };
  }
  if (input.change === null) {
    return {
      kind: "unavailable",
      reason: "No attempt has posted a diff for this issue, and it has no change request yet.",
    };
  }
  if (input.codeAvailability !== "available") {
    return { kind: "unavailable", reason: "The code artifact for this round is not available." };
  }
  return { kind: "version" };
}

export interface IssueSurfaces {
  readonly loading: boolean;
  readonly error: string | null;
  /** Null when the conversation is not linked to an issue. */
  readonly change: ChangeDetail | null;
  readonly changedFiles: readonly ChangedFile[];
  /**
   * The latest stored attempt diff on this issue, or null when no attempt has
   * posted one (decisions.md §18.5).
   */
  readonly attemptDiff: AttemptDiff | null;
  /** Which of the two sources the surface draws, decided by `diffSource`. */
  readonly source: DiffSource;
  readonly pullRequest: PullRequestSurfaceData | null;
  readonly agents: AgentPanelModel;
  readonly reload: () => void;
}

const NO_ATTEMPTS: readonly NativeAttempt[] = [];

/** The hub's attempt record as the Agents surface reads it. */
export function toAttemptActivity(
  attempts: readonly NativeAttempt[],
): readonly AttemptActivity[] {
  return attempts.map((attempt, index) => ({
    id: attempt.attempt_id,
    status: attempt.status,
    running: attempt.status === "running",
    runner: attempt.runner_id ?? attempt.machine_id ?? null,
    backend: attempt.identity?.backend ?? null,
    model: attempt.identity?.model ?? null,
    effort: null,
    access: null,
    startedAt: attempt.started_at,
    completedAt: attempt.status === "running" ? null : attempt.updated_at,
    tokens: null,
    attemptNumber: index + 1,
    progress: attempt.outcome ?? null,
    error: attempt.status === "failed" ? (attempt.outcome ?? null) : null,
  }));
}

function pullRequestState(summary: string | undefined): PullRequestState {
  if (summary === "merged") return "merged";
  if (summary === "closed" || summary === "abandoned") return "closed";
  return "open";
}

/**
 * The latest stored diff on this issue (decisions.md §18.5), or null when no
 * attempt has posted one.
 *
 * One request, issue-addressed. The attempt-addressed read of §18.5 answers a
 * 404 for an attempt that never checkpointed, so asking it per attempt would
 * mean a walk down the attempt list and a browser console error for each miss
 * — on every issue open, not only when the Diff surface is on screen. The hub
 * answers the issue-addressed read with `{diff: null}` instead.
 */
export async function readAttemptDiff(
  http: WorkHttp,
  projectId: string,
  workItemId: string,
): Promise<AttemptDiff | null> {
  const answer = await http.getWorkItemDiff(projectId, workItemId).catch(() => null);
  const diff = answer?.diff ?? null;
  return diff !== null && diff.files.length > 0 ? diff : null;
}

async function read(
  http: WorkHttp,
  projectId: string,
  workItemId: string,
): Promise<Omit<IssueSurfaces, "loading" | "error" | "reload">> {
  const [attempts, changes] = await Promise.all([
    http
      .listAttempts(projectId, workItemId, 20)
      .then((page) => page.items)
      .catch(() => NO_ATTEMPTS),
    http.listChanges(projectId, workItemId).catch(() => []),
  ]);
  const latest = changes.at(-1);
  const change =
    latest === undefined
      ? null
      : await http.getChange(projectId, workItemId, latest.change_id).catch(() => null);
  const version =
    change === null
      ? null
      : (change.versions.find(
          (candidate) => candidate.version_id === change.change.current_version_id,
        ) ??
        change.versions.at(-1) ??
        null);
  const external = version?.external ?? null;
  const number = external === null ? Number.NaN : Number.parseInt(external.id, 10);
  const attemptDiff = await readAttemptDiff(http, projectId, workItemId);
  return {
    change,
    // The hub serves no file list, hunk or patch for a change version, so this
    // is always empty today. The shape is the one a future
    // `GET …/changes/:change/files` would fill; the files a reader actually
    // sees come from `attemptDiff` instead (§18.5).
    changedFiles: [],
    attemptDiff,
    source: diffSource({
      attemptDiff,
      change,
      codeAvailability: version?.code?.availability ?? null,
      linkedToIssue: true,
    }),
    pullRequest:
      external === null || !Number.isFinite(number)
        ? null
        : {
            number,
            repository: version?.repository ?? "",
            url: external.url,
            state: pullRequestState(change?.summary.status),
            isDraft: change?.summary.status === "draft",
            title: change?.change.title ?? "",
          },
    agents: agentPanelModelFromAttempts(toAttemptActivity(attempts)),
  };
}

const EMPTY: Omit<IssueSurfaces, "loading" | "error" | "reload"> = {
  change: null,
  changedFiles: [],
  attemptDiff: null,
  source: diffSource({
    attemptDiff: null,
    change: null,
    codeAvailability: null,
    linkedToIssue: false,
  }),
  pullRequest: null,
  agents: agentPanelModelFromAttempts([]),
};

/**
 * How long a burst of project activity is coalesced into one re-read. The
 * board's own stream reader uses the same number for the same reason: a runner
 * that checkpoints and finishes inside one tick is one refresh, not two.
 */
const REFRESH_COALESCE_MS = 400;

/**
 * Everything the live right-panel surfaces show for one issue. Loaded once per
 * issue, re-read on demand, and re-read again when the project's event stream
 * says something happened.
 *
 * The diff has to follow the run (§18.5): the runner posts its worktree diff
 * before every `run.checkpointed` and before `run.finished`, and a reader
 * watching a running attempt should see each one land rather than having to
 * press Reload. The hub emits no typed project event for a run event, so the
 * signal available here is the project stream's `activity` frame — the issue
 * table's highest event sequence, which a run event appended to this issue's
 * history bumps. It is written on every tick whether or not it moved, so only
 * an increase is acted on, exactly as `useBoard` does with the same frame.
 */
export function useIssueSurfaces(
  projectId: string | null,
  workItemId: string | null,
): IssueSurfaces {
  const http = useWorkHttp();
  const [state, setState] = React.useState(EMPTY);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [nonce, setNonce] = React.useState(0);

  React.useEffect(() => {
    if (projectId === null || workItemId === null) return;
    if (typeof globalThis.EventSource !== "function") return;
    const source = new globalThis.EventSource(http.eventsUrl(projectId), {
      withCredentials: true,
    });
    let previous: number | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const onActivity = (event: MessageEvent<string>) => {
      const next = Number.parseInt(event.data, 10);
      if (!Number.isFinite(next)) return;
      const seen = previous;
      previous = next;
      if (seen === null || next <= seen) return;
      clearTimeout(timer);
      timer = setTimeout(() => setNonce((value) => value + 1), REFRESH_COALESCE_MS);
    };
    source.addEventListener("activity", onActivity as EventListener);
    return () => {
      clearTimeout(timer);
      source.removeEventListener("activity", onActivity as EventListener);
      source.close();
    };
  }, [http, projectId, workItemId]);

  React.useEffect(() => {
    if (projectId === null || workItemId === null) {
      setState(EMPTY);
      setLoading(false);
      setError(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    void read(http, projectId, workItemId)
      .then((next) => {
        if (cancelled) return;
        setState(next);
        setError(null);
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        setError(cause instanceof Error ? cause.message : String(cause));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [http, projectId, workItemId, nonce]);

  return React.useMemo(
    () => ({ ...state, loading, error, reload: () => setNonce((value) => value + 1) }),
    [state, loading, error],
  );
}
