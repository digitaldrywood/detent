import type { Conversation } from "../../contracts/conversation.ts";
import { turnPreferences } from "../../contracts/conversation.ts";
import { HUB_ENVIRONMENT_ID } from "../../contracts/index.ts";
import type {
  OrchestrationLatestTurn,
  OrchestrationSession,
  OrchestrationSessionStatus,
  ProjectId,
  RuntimeMode,
  ThreadId,
  TurnId,
} from "../../contracts/ui.ts";
import type { EnvironmentThreadShell } from "./models.ts";
import { resolveSettledOverride, type SettledOverrides } from "./settledOverrides.ts";

/** Detent's lanes (U08), as the sidebar row's branch slot names them. */
export type IssueLane = "Needs you" | "Running" | "Waiting";

export const ISSUE_LANES: readonly IssueLane[] = ["Needs you", "Running", "Waiting"];

/**
 * Which lane a conversation belongs to. A conversation whose attempt has ended
 * has no lane: it is not waiting on a runner and it is not waiting on the
 * reader. `waiting_for_runner` stands alone (A.11): a queue signal and a
 * working runner must never share a treatment.
 */
export function issueLane(conversation: Conversation): IssueLane | null {
  switch (conversation.execution.status) {
    case "waiting_input":
    case "failed":
    case "unknown":
      return "Needs you";
    case "starting":
    case "running":
    case "interrupting":
      return "Running";
    case "waiting_for_runner":
      return "Waiting";
    case "idle":
    case "completed":
    case "interrupted":
      return null;
  }
}

export function issueNumberLabel(identifier: string): string {
  const hash = identifier.lastIndexOf("#");
  return hash === -1 ? identifier : identifier.slice(hash);
}

/**
 * The same identifier with the join repaired: "Browser collaboration#2" reads
 * "Browser collaboration #2". Used where the whole identifier is shown rather
 * than only its number.
 */
export function issueIdentifierLabel(identifier: string): string {
  const hash = identifier.lastIndexOf("#");
  if (hash <= 0) return identifier;
  const project = identifier.slice(0, hash).trimEnd();
  return `${project} ${identifier.slice(hash)}`;
}

export function laneBranchLabel(conversation: Conversation): string | null {
  const lane = issueLane(conversation);
  const identifier = conversation.work_item?.identifier;
  if (identifier !== undefined) {
    return lane === null
      ? issueNumberLabel(identifier)
      : `${issueNumberLabel(identifier)} · ${lane}`;
  }
  return lane;
}

function sessionStatus(conversation: Conversation): OrchestrationSessionStatus {
  switch (conversation.execution.status) {
    case "starting":
      return "starting";
    case "running":
    case "interrupting":
      return "running";
    case "failed":
    case "unknown":
      return "error";
    case "interrupted":
      return "interrupted";
    case "completed":
      return "stopped";
    case "waiting_input":
    case "waiting_for_runner":
    case "idle":
      return "idle";
  }
}

function backgroundLiveness(conversation: Conversation): "working" | "monitoring" | null {
  return conversation.execution.status === "waiting_for_runner" ? "monitoring" : null;
}

function runtimeMode(conversation: Conversation): RuntimeMode {
  switch (turnPreferences(conversation).access) {
    case "read_only":
      return "approval-required";
    case "full":
      return "full-access";
    default:
      return "auto";
  }
}

function latestTurn(conversation: Conversation): OrchestrationLatestTurn | null {
  const execution = conversation.execution;
  if (execution.turn_id === null && execution.attempt_id === null) return null;
  const live =
    execution.status === "starting" ||
    execution.status === "running" ||
    execution.status === "interrupting";
  const state =
    execution.status === "failed" || execution.status === "unknown"
      ? "error"
      : execution.status === "interrupted"
        ? "interrupted"
        : live
          ? "running"
          : "completed";
  return {
    turnId: (execution.turn_id ?? execution.attempt_id ?? conversation.id) as TurnId,
    state,
    requestedAt: execution.updated_at,
    startedAt: live ? execution.updated_at : null,
    completedAt: live ? null : execution.updated_at,
    assistantMessageId: null,
  };
}

function session(conversation: Conversation): OrchestrationSession {
  return {
    threadId: conversation.id as ThreadId,
    status: sessionStatus(conversation),

    providerName: conversation.execution.runner_id,
    runtimeMode: runtimeMode(conversation),
    activeTurnId: (conversation.execution.turn_id as TurnId | null) ?? null,
    lastError: conversation.execution.error,
    updatedAt: conversation.execution.updated_at,
  };
}

export interface ThreadShellOptions {
  /** Conversation ids the attention list names as needing the reader (B.4). */
  readonly attention?: ReadonlySet<string> | undefined;
  /** The reader's own settle / un-settle clicks (`settledOverrides.ts`). */
  readonly settledOverrides?: SettledOverrides | undefined;
}

const NO_OVERRIDES: SettledOverrides = { settledAtById: {}, unsettledAtById: {} };

/** One conversation, as the copied sidebar's row reads it. */
export function toEnvironmentThreadShell(
  conversation: Conversation,
  options: ThreadShellOptions = {},
): EnvironmentThreadShell {
  const settledOverride = resolveSettledOverride(
    conversation,
    options.settledOverrides ?? NO_OVERRIDES,
  );
  const settled = settledOverride === "settled";
  const preferences = turnPreferences(conversation);
  return {
    environmentId: HUB_ENVIRONMENT_ID,
    id: conversation.id as ThreadId,
    projectId: conversation.project_id as ProjectId,
    title: conversation.title,
    modelSelection: { instanceId: preferences.model, model: preferences.model },
    runtimeMode: runtimeMode(conversation),
    interactionMode: "default",

    branch: laneBranchLabel(conversation),
    worktreePath: null,
    linkedPullRequest: null,
    branchPullRequest: null,
    latestTurn: latestTurn(conversation),
    createdAt: conversation.created_at,
    updatedAt: conversation.updated_at,
    // §14 removed the archived status outright; nothing is ever archived.
    archivedAt: null,
    settledOverride,
    settledAt: settled ? conversation.updated_at : null,
    session: session(conversation),
    latestUserMessageAt: conversation.last_message_at,

    hasPendingApprovals: false,
    hasPendingUserInput:
      conversation.execution.status === "waiting_input" ||
      (options.attention?.has(conversation.id) ?? false),
    hasActionableProposedPlan: false,
    backgroundLiveness: backgroundLiveness(conversation),
  };
}
