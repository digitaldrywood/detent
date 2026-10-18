// Execution status, in plain language, plus which controls it allows.
//
// Written for this repository. Design inventory A.10 ("Execution control
// strip") and A.11 require that waiting for a runner and a working runner
// never share a treatment, so every status here carries its own sentence and
// its own tone rather than collapsing into "busy".
//
// The affordances are read from `execution.capabilities`, which the hub sets
// from the backend that is actually bound. A control the backend cannot honour
// is not rendered, because rendering it would promise something the client
// cannot deliver (decisions.md §5, `unsupported_control`).
import type { Execution, ExecutionStatus } from "../../contracts/index.ts";

export type Tone = "dc-ok" | "dc-warn" | "dc-err" | "dc-info" | "";

/**
 * Which surface the copy is written for. A linked conversation is an issue a
 * runner works on; an unlinked chat is a question a runner answers. Every model
 * turn runs on a customer runner either way (decisions.md §1, "Where model
 * turns execute"), so the difference is what the runner is doing, not whether
 * one is involved — and saying "issue" in a chat that has none is a lie the
 * reader has to decode.
 */
export type ExecutionSurface = "issue" | "chat";

export interface ExecutionCopy {
  readonly label: string;
  readonly tone: Tone;
  /** One sentence saying what is true now, not what the system is doing. */
  readonly sentence: string;
}

const COPY: Record<ExecutionStatus, ExecutionCopy> = {
  idle: {
    label: "Idle",
    tone: "",
    sentence: "No runner is working on this issue.",
  },
  waiting_for_runner: {
    label: "Waiting for a runner",
    tone: "dc-warn",
    sentence: "The issue is queued. Nothing is running yet.",
  },
  starting: {
    label: "Starting",
    tone: "dc-info",
    sentence: "A runner took the issue and is starting its attempt.",
  },
  running: {
    label: "Running",
    tone: "dc-ok",
    sentence: "A runner is working on this issue now.",
  },
  waiting_input: {
    label: "Waiting for you",
    tone: "dc-warn",
    sentence: "The runner stopped to ask you something.",
  },
  interrupting: {
    label: "Interrupting",
    tone: "dc-warn",
    sentence: "Your interrupt is on its way to the runner.",
  },
  completed: {
    label: "Completed",
    tone: "dc-ok",
    sentence: "The last attempt finished.",
  },
  interrupted: {
    label: "Interrupted",
    tone: "dc-warn",
    sentence: "The last attempt stopped when it was interrupted.",
  },
  failed: {
    label: "Failed",
    tone: "dc-err",
    sentence: "The last attempt failed.",
  },
  unknown: {
    label: "Unknown",
    tone: "dc-warn",
    sentence: "We cannot read the runner's state. Refresh to check.",
  },
};

/**
 * The same ladder for an unlinked chat. `waiting_for_runner` names the project,
 * because that is where a runner has to be enrolled for the chat to be answered
 * at all (decisions.md §9.2), and A.11 still holds: waiting for a runner and a
 * runner that is answering never share a treatment.
 */
const CHAT_COPY: Record<ExecutionStatus, ExecutionCopy> = {
  idle: {
    label: "Idle",
    tone: "",
    sentence: "No runner is answering this chat.",
  },
  waiting_for_runner: {
    label: "Waiting for a runner",
    tone: "dc-warn",
    sentence: "Waiting for a runner to pick up this chat.",
  },
  starting: {
    label: "Starting",
    tone: "dc-info",
    sentence: "A runner is answering this chat.",
  },
  running: {
    label: "Running",
    tone: "dc-ok",
    sentence: "A runner is answering this chat.",
  },
  waiting_input: {
    label: "Waiting for you",
    tone: "dc-warn",
    sentence: "The runner stopped to ask you something.",
  },
  interrupting: {
    label: "Stopping",
    tone: "dc-warn",
    sentence: "Your stop is on its way to the runner.",
  },
  completed: {
    label: "Completed",
    tone: "dc-ok",
    sentence: "The runner finished answering.",
  },
  interrupted: {
    label: "Stopped",
    tone: "dc-warn",
    sentence: "The answer stopped when you stopped it.",
  },
  failed: {
    label: "Failed",
    tone: "dc-err",
    sentence: "The runner could not answer this chat.",
  },
  unknown: {
    label: "Unknown",
    tone: "dc-warn",
    sentence: "We cannot read the runner's state. Refresh to check.",
  },
};

export interface ExecutionCopyOptions {
  readonly surface?: ExecutionSurface;
  /** Named where the copy points at a project, e.g. waiting for a runner. */
  readonly projectName?: string | null;
}

export function executionCopy(
  execution: Execution,
  options: ExecutionCopyOptions = {},
): ExecutionCopy {
  const chat = options.surface === "chat";
  const base = chat ? CHAT_COPY[execution.status] : COPY[execution.status];
  let sentence = base.sentence;
  const project = options.projectName ?? "";
  if (chat && execution.status === "waiting_for_runner" && project.length > 0) {
    sentence = `Waiting for a runner to pick up this chat in ${project}.`;
  }
  if (execution.status === "failed" && execution.error !== null && execution.error.length > 0) {
    sentence = `${sentence} ${execution.error}`;
  }
  return sentence === base.sentence ? base : { ...base, sentence };
}

/**
 * The statuses whose sentence only restates the label.
 *
 * "Completed · The last attempt finished." is the label twice, and next to the
 * attempt id it wrapped the strip onto a second line (Michael's review,
 * September 11). The sentences are not deleted — `executionCopy` still returns
 * them, and the issue page still reads them where it has a paragraph's room —
 * but the strip is one line, so it prints only the ones that say something the
 * label does not.
 *
 * What stays: waiting for a runner (queued is not idle, and the chat copy
 * names the project), waiting for you (why it stopped), unknown (what to do),
 * and a failure that came with a reason. A.11 still holds — a queued issue and
 * a working runner share neither label, tone nor sentence.
 */
const LABEL_IS_THE_WHOLE_SENTENCE: ReadonlySet<ExecutionStatus> = new Set<ExecutionStatus>([
  "idle",
  "starting",
  "running",
  "interrupting",
  "completed",
  "interrupted",
]);

/**
 * The sentence the one-line strip prints beside the label, or null when the
 * label already says it. A failure prints the reason the hub gave, which is
 * the whole of what the sentence was carrying.
 */
export function executionStripSentence(
  execution: Execution,
  options: ExecutionCopyOptions = {},
): string | null {
  if (LABEL_IS_THE_WHOLE_SENTENCE.has(execution.status)) return null;
  if (execution.status === "failed") {
    const reason = execution.error ?? "";
    return reason.length === 0 ? null : reason;
  }
  return executionCopy(execution, options).sentence;
}

/** Statuses where the attempt has ended. `unknown` is not one of them. */
export const TERMINAL_STATUSES: ReadonlySet<ExecutionStatus> = new Set<ExecutionStatus>([
  "completed",
  "interrupted",
  "failed",
]);

export function isTerminal(status: ExecutionStatus): boolean {
  return TERMINAL_STATUSES.has(status);
}

/** True while a runner is producing output or holding the turn open. */
export function isActive(status: ExecutionStatus): boolean {
  return status === "starting" || status === "running" || status === "waiting_input";
}

/**
 * An interrupt names the attempt it is aimed at, so it needs one. Interrupting
 * an interrupt is not offered: the first one is already in flight.
 */
export function canInterrupt(execution: Execution): boolean {
  return (
    execution.capabilities.interrupt &&
    execution.attempt_id !== null &&
    isActive(execution.status)
  );
}

/**
 * `continue` records intent and reports `waiting_for_runner`; it never starts
 * a runner from the hub (decisions.md §5). It is offered only once the
 * previous attempt has ended, or when nothing has started yet.
 */
export function canContinue(execution: Execution): boolean {
  return (
    execution.capabilities.continue &&
    (isTerminal(execution.status) || execution.status === "idle")
  );
}

/**
 * An unlinked chat's one control is `cancel`, which the hub delivers as an
 * interrupt to the runner holding the coordinator work item (decisions.md
 * §9.1); the client-facing kind stays `cancel`. It needs no attempt id and no
 * `capabilities.interrupt`: the capability set belongs to a bound issue
 * backend, and a chat that is producing output can always be stopped.
 */
export function canStopChat(execution: Execution): boolean {
  return execution.status === "starting" || execution.status === "running";
}

/** The expected owner generation a control must carry. */
export function expectedOwner(execution: Execution): {
  readonly attempt_id: string | null;
  readonly turn_id: string | null;
} {
  return { attempt_id: execution.attempt_id, turn_id: execution.turn_id };
}
