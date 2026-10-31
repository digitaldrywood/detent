export type RuntimeSubagentStatus =
  | "pending"
  | "running"
  | "waiting"
  | "idle"
  | "completed"
  | "failed"
  | "cancelled"
  | "interrupted";

export interface SubagentUsage {
  readonly totalTokens: number;
  readonly inputTokens?: number;
  readonly cachedInputTokens?: number;
  readonly outputTokens?: number;
  readonly reasoningOutputTokens?: number;
  readonly toolUses?: number;
  readonly durationMs?: number;
}

export interface SubagentActivityEntry {
  readonly at: string;
  readonly summary: string;
}

export interface SubagentWorkflowPhase {
  readonly index: number;
  readonly title: string;
}

export interface SubagentRunHandles {
  readonly runId?: string;
  readonly scriptPath?: string;
  readonly transcriptDir?: string;
  readonly sessionUrl?: string;
}

export interface RuntimeSubagent {
  readonly id: string;
  readonly kind: "subagent" | "subagent_batch" | "workflow" | "workflow_agent";
  readonly title: string;
  readonly role: string | null;
  readonly model: string | null;
  readonly effort: string | null;
  readonly status: RuntimeSubagentStatus;
  readonly activationCount: number;
  readonly usage: SubagentUsage | null;
  readonly progress: string | null;
  readonly lastToolName: string | null;
  readonly result: string | null;
  readonly error: string | null;
  readonly outputFile: string | null;
  readonly parentAgentId: string | null;
  readonly agentIndex: number | null;
  readonly phaseIndex: number | null;
  readonly phaseTitle: string | null;
  readonly attempt: number | null;
  readonly workflowName: string | null;
  readonly phases: ReadonlyArray<SubagentWorkflowPhase>;
  readonly runHandles: SubagentRunHandles | null;
  readonly recentActivity: ReadonlyArray<SubagentActivityEntry>;
  /** First retained observation, used as the roster's stable display order. */
  readonly firstSeenAt: string;
  readonly startedAt: string | null;
  readonly completedAt: string | null;
  readonly updatedAt: string;
}

export interface AgentPanelWorkflowGroup {
  readonly workflow: RuntimeSubagent;
  readonly phases: ReadonlyArray<{
    readonly index: number;
    readonly title: string;
    readonly members: ReadonlyArray<RuntimeSubagent>;
    /** done = every member settled (success or error); running = any active. */
    readonly state: "pending" | "running" | "done";
    readonly activeCount: number;
    readonly settledCount: number;
  }>;
  /** Members with no resolvable phase (orphans render under the workflow). */
  readonly unphasedMembers: ReadonlyArray<RuntimeSubagent>;
}

export interface AgentPanelModel {
  readonly workflows: ReadonlyArray<AgentPanelWorkflowGroup>;
  readonly directAgents: ReadonlyArray<RuntimeSubagent>;
  readonly runningCount: number;
  readonly waitingCount: number;
  readonly idleCount: number;
  readonly settledCount: number;
  readonly totalTokens: number;
  readonly hasAgents: boolean;
  readonly liveCount: number;
}

const EMPTY_PANEL_MODEL: AgentPanelModel = {
  workflows: [],
  directAgents: [],
  runningCount: 0,
  waitingCount: 0,
  idleCount: 0,
  settledCount: 0,
  totalTokens: 0,
  hasAgents: false,
  liveCount: 0,
};

export function emptyAgentPanelModel(): AgentPanelModel {
  return EMPTY_PANEL_MODEL;
}

const TERMINAL_STATUSES: ReadonlySet<RuntimeSubagentStatus> = new Set([
  "completed",
  "failed",
  "cancelled",
  "interrupted",
]);

export function isTerminalSubagentStatus(status: RuntimeSubagentStatus): boolean {
  return TERMINAL_STATUSES.has(status);
}

/** Active = the user may still need to care while it runs. Idle is settled-ish
 * but resumable; waiting counts as active because it needs the user. */
export function isActiveSubagentStatus(status: RuntimeSubagentStatus): boolean {
  return status === "pending" || status === "running" || status === "waiting";
}

export function formatSubagentModelLabel(
  model: string | null,
  effort: string | null,
): string | null {
  if (!model) {
    return null;
  }
  const compact = model
    .replace(/^claude-/, "")
    .replace(/-\d{8}$/, "")
    .replace(/-latest$/, "");
  return effort ? `${compact} · ${effort}` : compact;
}

export function formatSubagentTokenCount(totalTokens: number): string {
  if (totalTokens < 1000) {
    return `${totalTokens}`;
  }
  if (totalTokens < 1_000_000) {
    const value = totalTokens / 1000;
    return `${value >= 100 ? Math.round(value) : value.toFixed(1)}k`;
  }
  return `${(totalTokens / 1_000_000).toFixed(1)}M`;
}

// ---------------------------------------------------------------------------
// Detent's side of the boundary.

/** What the Agents surface is fed: one attempt on a customer runner. */
export interface AttemptActivity {
  readonly id: string;
  readonly status: string;
  readonly running: boolean;
  readonly runner: string | null;
  readonly backend: string | null;
  readonly model: string | null;
  readonly effort: string | null;
  readonly access: string | null;
  readonly startedAt: string | null;
  readonly completedAt?: string | null;
  readonly tokens: number | null;
  readonly attemptNumber: number | null;
  /** The most recent thing the runner reported, if the hub has one. */
  readonly progress?: string | null;
  readonly error?: string | null;
}

export function attemptStatus(status: string, running: boolean): RuntimeSubagentStatus {
  switch (status.trim().toLowerCase()) {
    case "running":
    case "starting":
      return "running";
    case "queued":
    case "pending":
    case "scheduled":
      return "pending";
    case "waiting":
    case "waiting_input":
    case "waiting_for_runner":
      return "waiting";
    case "succeeded":
    case "completed":
    case "finished":
      return "completed";
    case "failed":
    case "errored":
      return "failed";
    case "cancelled":
    case "canceled":
      return "cancelled";
    case "interrupted":
      return "interrupted";
    default:
      return running ? "running" : "idle";
  }
}

function toRuntimeSubagent(attempt: AttemptActivity): RuntimeSubagent {
  const status = attemptStatus(attempt.status, attempt.running);
  const settled =
    status === "completed" ||
    status === "failed" ||
    status === "cancelled" ||
    status === "interrupted";
  const runner = attempt.runner ?? attempt.backend;
  return {
    id: attempt.id,
    kind: "subagent",
    title:
      attempt.attemptNumber === null ? "Runner attempt" : `Attempt ${attempt.attemptNumber}`,
    role: runner,
    model: attempt.model,
    effort: attempt.effort,
    status,
    activationCount: attempt.attemptNumber ?? 1,
    usage: attempt.tokens === null ? null : { totalTokens: attempt.tokens },
    progress: attempt.progress ?? null,
    lastToolName: null,
    result: settled && attempt.error == null ? attempt.status : null,
    error: attempt.error ?? null,
    outputFile: null,
    parentAgentId: null,
    agentIndex: null,
    phaseIndex: null,
    phaseTitle: null,
    attempt: attempt.attemptNumber,
    workflowName: null,
    phases: [],

    runHandles: null,
    recentActivity: [],
    firstSeenAt: attempt.startedAt ?? "",
    startedAt: attempt.startedAt,
    completedAt: attempt.completedAt ?? null,
    updatedAt: attempt.completedAt ?? attempt.startedAt ?? "",
  };
}

/**
 * The panel model for one issue's runner activity. Every attempt is a direct
 * spawn: the hub reports no parent/child relationship between attempts, so
 * inventing a workflow group for them would be a shape the data does not have.
 */
export function agentPanelModelFromAttempts(
  attempts: readonly AttemptActivity[],
): AgentPanelModel {
  if (attempts.length === 0) return EMPTY_PANEL_MODEL;
  const agents = attempts.map(toRuntimeSubagent);
  let runningCount = 0;
  let waitingCount = 0;
  let idleCount = 0;
  let settledCount = 0;
  let totalTokens = 0;
  for (const agent of agents) {
    totalTokens += agent.usage?.totalTokens ?? 0;
    switch (agent.status) {
      case "running":
      case "pending":
        runningCount += 1;
        break;
      case "waiting":
        waitingCount += 1;
        break;
      case "idle":
        idleCount += 1;
        break;
      default:
        settledCount += 1;
    }
  }
  return {
    workflows: [],
    directAgents: agents,
    runningCount,
    waitingCount,
    idleCount,
    settledCount,
    totalTokens,
    hasAgents: true,
    liveCount: runningCount + waitingCount,
  };
}
