// What a live surface *says* while its workspace session is not serving it
// (decisions.md §18.1).
//
// §18.1 gives a workspace eight states and nine reasons, and a surface that
// draws a skeleton for all of them tells a reader nothing: a request no runner
// claims sits in `requested` for `workspaces.request_timeout` — five minutes by
// default — and a panel that answers that with grey bars for five minutes and
// then more grey bars is indistinguishable from one that is broken. So every
// state that is not serving is a sentence here, and the skeleton is kept for
// exactly one state: `starting`, where the worktree really is being written and
// the rows really are about to arrive.
//
// The mapping lives in its own module rather than in the surfaces because the
// Files surface and the Output surface wait on the same session for different
// reasons, and two copies of nine sentences drift. What differs between them is
// only the capability they need the runner to have reported, which is the one
// parameter the sentences take.
import type { Workspace, WorkspaceReason, WorkspaceState } from "../../contracts/work.ts";

/** The channel a surface needs the claiming runner to have reported (§18.12). */
export type WorkspaceCapability = "files" | "exec" | "terminal";

interface CapabilityWords {
  /** The capability as the hub names it, for the `no_runner` sentence. */
  readonly name: string;
  /** "…a runner that can <serve>…" */
  readonly serve: string;
  /** "…this workspace will <resume> if the runner comes back." */
  readonly resume: string;
}

const CAPABILITY_WORDS: Record<WorkspaceCapability, CapabilityWords> = {
  files: {
    name: "files",
    serve: "serve files for this project",
    resume: "serve files again",
  },
  terminal: {
    name: "terminal",
    serve: "open a terminal on this project",
    resume: "serve terminals again",
  },
  exec: {
    name: "exec",
    serve: "run commands for this project",
    resume: "run commands again",
  },
};

/**
 * The facts about the session the sentences need, pulled off the resource.
 *
 * Kept as its own shape so a surface takes one optional prop rather than four,
 * and so a test can state a runner or a request time without a whole workspace.
 */
export interface WorkspaceSessionFacts {
  readonly runnerId?: string | null;
  readonly machineId?: string | null;
  /** When the request was made; the elapsed clock counts from here. */
  readonly requestedAt?: string | null;
  /** The request deadline, where the hub exposes one. */
  readonly requestedExpiresAt?: string | null;
  /** When the session left the happy path, for deriving the request timeout. */
  readonly endedAt?: string | null;
}

/**
 * The session facts a `Workspace` carries.
 *
 * `requested_expires_at` is on the hub's record (`workspaceRecord`) but not on
 * the resource it serves, so the request timeout is derived the only way a
 * client can: for a request that failed with `no_runner`, the gap between when
 * it was created and when it failed *is* `workspaces.request_timeout`. Where
 * that cannot be worked out the sentence says so plainly rather than naming a
 * number this client guessed.
 */
export function workspaceSessionFacts(workspace: Workspace | null): WorkspaceSessionFacts | null {
  if (workspace === null) return null;
  return {
    runnerId: workspace.runner_id ?? null,
    machineId: workspace.machine_id ?? null,
    requestedAt: workspace.created_at,
    requestedExpiresAt: null,
    endedAt: workspace.updated_at,
  };
}

/** Which shape of waiting this is; the surface keys its test id off it. */
export type WorkspaceStatusKind =
  | "opening"
  | "waiting"
  | "starting"
  | "unreachable"
  | "closing"
  | "connecting"
  | "unavailable";

export interface WorkspaceStatusPresentation {
  readonly kind: WorkspaceStatusKind;
  /** The words in the empty area. Always rendered, never only an aria-label. */
  readonly sentence: string;
  /** True for `starting` alone: the one wait with an end a reader can see. */
  readonly skeleton: boolean;
  /** An ISO time to count up from, or null where elapsed means nothing. */
  readonly elapsedSince: string | null;
  /** The retry affordance's label, or null where retrying is not the answer. */
  readonly retryLabel: string | null;
  /** Suffix of the surface's own test id: `files-waiting`, `output-waiting`. */
  readonly testId: string;
}

export interface WorkspaceStatusInput {
  /** Null before the hub has answered at all. */
  readonly state: WorkspaceState | null;
  readonly reason: WorkspaceReason | null;
  readonly capability: WorkspaceCapability;
  readonly session?: WorkspaceSessionFacts | null;
  /** False while a live workspace's relay socket is not open yet. */
  readonly connected?: boolean;
}

const RETRY_LABEL = "Retry";
const NEW_WORKSPACE_LABEL = "Open a new workspace";

/**
 * A live workspace whose relay socket is not open yet. Exported because it is
 * also the answer to "the session says ready and the surface has no client",
 * which a surface can reach without going back through the state machine.
 */
export const WORKSPACE_CONNECTING_STATUS: WorkspaceStatusPresentation = {
  kind: "connecting",
  sentence: "Connecting to the workspace…",
  skeleton: false,
  elapsedSince: null,
  retryLabel: null,
  testId: "connecting",
};

/** The runner a sentence can name, or null when nothing has claimed it yet. */
export function workspaceRunnerName(session: WorkspaceSessionFacts | null | undefined): string | null {
  const runner = session?.runnerId ?? null;
  if (runner !== null && runner.length > 0) return runner;
  const machine = session?.machineId ?? null;
  if (machine !== null && machine.length > 0) return machine;
  return null;
}

function parseTime(value: string | null | undefined): number | null {
  if (value === null || value === undefined || value.length === 0) return null;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? parsed : null;
}

/**
 * "1m 20s" — the elapsed clock under a wait, in the coarsest pair of units
 * that still moves. Seconds are dropped past an hour: a reader watching a wait
 * that long is not counting seconds.
 */
export function formatElapsed(milliseconds: number): string {
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return "0s";
  const total = Math.floor(milliseconds / 1000);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m ${seconds}s`;
  return `${seconds}s`;
}

/**
 * The request timeout as a phrase, derived from the resource, or null.
 *
 * Preference order is the deadline the hub stated, then the gap a failed
 * request actually waited. Null means the sentence must not name a number.
 */
export function workspaceRequestTimeoutPhrase(
  session: WorkspaceSessionFacts | null | undefined,
): string | null {
  const requested = parseTime(session?.requestedAt);
  if (requested === null) return null;
  const deadline = parseTime(session?.requestedExpiresAt) ?? parseTime(session?.endedAt);
  if (deadline === null) return null;
  const milliseconds = deadline - requested;
  if (milliseconds <= 0) return null;
  const seconds = Math.round(milliseconds / 1000);
  if (seconds < 60) return `${seconds} second${seconds === 1 ? "" : "s"}`;
  // The sweep notices a deadline shortly after it passes, so the gap is the
  // configured timeout plus a little; the nearest minute is the honest reading
  // of it, and is what an operator would recognise from their own config.
  const minutes = Math.max(1, Math.round(seconds / 60));
  return `${minutes} minute${minutes === 1 ? "" : "s"}`;
}

export interface WorkspaceReasonOptions {
  /** Names the capability in the `no_runner` sentence. */
  readonly capability?: WorkspaceCapability;
  /** The request timeout, already phrased ("5 minutes"). */
  readonly requestTimeout?: string | null;
}

/**
 * The sentence a reader sees for a workspace that failed or closed (§18.1).
 *
 * Every reason gets its own; the fallbacks are for a build that meets a reason
 * it does not know, which is a decode this client survives rather than a state
 * it can describe.
 */
export function workspaceReasonSentence(
  state: WorkspaceState,
  reason: WorkspaceReason | null,
  options: WorkspaceReasonOptions = {},
): string {
  switch (reason) {
    case "no_runner": {
      const capability = options.capability;
      if (capability === undefined) {
        return "No runner reported the capabilities this workspace needs, so it timed out waiting.";
      }
      const name = CAPABILITY_WORDS[capability].name;
      const timeout = options.requestTimeout ?? null;
      return timeout === null
        ? `No runner reported the ${name} capability before the request timed out.`
        : `No runner reported the ${name} capability within ${timeout}.`;
    }
    case "checkout_failed":
      return "The runner could not check the repository out.";
    case "worktree_missing":
      return "The worktree this workspace was attached to is gone.";
    case "runner_restarted":
      return "The runner restarted and released this worktree.";
    case "hub_restarted":
      return "The hub restarted and this workspace was not re-bound in time.";
    case "lease_lost":
      return "The runner stopped answering and lost its lease on the worktree.";
    case "capacity":
      return "The runner had no capacity left for this workspace.";
    case "closed_by_actor":
      return "This workspace was closed.";
    case "expired":
      return "This workspace reached its idle timeout and closed.";
    default:
      return state === "failed" ? "This workspace failed." : "This workspace has closed.";
  }
}

/**
 * What to draw instead of the surface's own content, or null when there is
 * nothing in the way and the surface should draw itself.
 */
export function describeWorkspaceStatus(
  input: WorkspaceStatusInput,
): WorkspaceStatusPresentation | null {
  const { state, reason, capability } = input;
  const session = input.session ?? null;
  const words = CAPABILITY_WORDS[capability];

  if (state === null) {
    // The hub has not answered yet. A surface with something else to say while
    // no workspace exists at all (the Output surface's "no runs yet") decides
    // that before it asks here.
    return {
      kind: "opening",
      sentence: "Opening a workspace for this conversation…",
      skeleton: false,
      elapsedSince: null,
      retryLabel: null,
      testId: "opening",
    };
  }

  switch (state) {
    case "requested":
      return {
        kind: "waiting",
        sentence: `Waiting for a runner that can ${words.serve}…`,
        skeleton: false,
        elapsedSince: session?.requestedAt ?? null,
        retryLabel: null,
        testId: "waiting",
      };
    case "starting": {
      const runner = workspaceRunnerName(session);
      return {
        kind: "starting",
        sentence:
          runner === null
            ? "Checking out the worktree on the runner…"
            : `Checking out the worktree on ${runner}…`,

        skeleton: true,
        elapsedSince: null,
        retryLabel: null,
        testId: "starting",
      };
    }
    case "unreachable":
      return {
        kind: "unreachable",
        sentence: `The runner stopped answering. This workspace will ${words.resume} if the runner comes back.`,
        skeleton: false,
        elapsedSince: null,
        retryLabel: null,
        testId: "unreachable",
      };
    case "closing":
      return {
        kind: "closing",
        sentence: "This workspace is closing.",
        skeleton: false,
        elapsedSince: null,
        retryLabel: null,
        testId: "closing",
      };
    case "failed":
      return {
        kind: "unavailable",
        sentence: workspaceReasonSentence("failed", reason, {
          capability,
          requestTimeout: workspaceRequestTimeoutPhrase(session),
        }),
        skeleton: false,
        elapsedSince: null,
        retryLabel: RETRY_LABEL,
        testId: "unavailable",
      };
    case "closed":
      return {
        kind: "unavailable",
        sentence: workspaceReasonSentence("closed", reason, { capability }),
        skeleton: false,
        elapsedSince: null,
        retryLabel: NEW_WORKSPACE_LABEL,
        testId: "unavailable",
      };
    default:
      // `ready` and `idle`: the session is up. The socket may still be opening,
      // which is a wait of its own and is also words rather than a skeleton.
      if (input.connected === false) return WORKSPACE_CONNECTING_STATUS;
      return null;
  }
}
