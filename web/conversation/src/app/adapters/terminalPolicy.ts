import {
  WORKSPACE_UNUSABLE_STATES,
  type WorkspaceReason,
  type WorkspaceState,
} from "../../contracts/work.ts";
import { workspaceReasonSentence } from "../lib/workspaceStatus.ts";

export interface TerminalPolicy {
  /** Null where there is no issue, so no worktree can exist at all (§18.1). */
  readonly workItemId: string | null;
  /** The project grant's write flag. */
  readonly canWrite: boolean;
  /** The project grant's `runners` flag, which §18.3 requires for a terminal. */
  readonly canManageRunners: boolean;
  /** The membership role. A viewer never gets a terminal, whatever the grant says. */
  readonly role: string;
  /** The attempt behind this workspace is still running, so it is read-only. */
  readonly workspaceReadOnly: boolean;
  readonly workspaceState: WorkspaceState | null;
  readonly workspaceReason?: WorkspaceReason | null | undefined;
  readonly workspaceError: string | null;
  /**
   * What the runner reported for this project, or null before anything has.
   *
   * It is the bootstrap's `projects[].capabilities.terminal` until a workspace
   * exists and the workspace's own afterwards, exactly as §18.10 says: "once a
   * workspace exists the panel reads the workspace's own capabilities, because
   * only that runner can serve it."
   */
  readonly terminalCapable: boolean | null;
}

/**
 * Every sentence this layer can say, in one place.
 *
 * One sentence each, ending in a full stop, naming the fact rather than the
 * fix — the same voice `GIT_POLICY_SENTENCES` uses, because a reader moving
 * between a disabled git group and a disabled terminal card should not be able
 * to tell two authors apart.
 */
export const TERMINAL_POLICY_SENTENCES = {
  noIssue: "Link an issue to get a worktree.",
  viewer: "Viewers cannot open a terminal.",
  noWriteGrant: "You need write access on this project to open a terminal.",
  noRunnerGrant: "You need the project's runner grant to open a terminal.",
  attemptRunning: "The attempt is still running, so this worktree is read-only.",
  disabled: "Terminals are turned off for this organization.",
  noCapability: "This runner does not serve terminals.",
} as const;

/**
 * Why the Terminal surface cannot be opened, or null.
 *
 * The order is most structural to least, and each check is a precondition of
 * the one after it: there is no point telling a reader their runner serves no
 * terminals when there is no worktree for it to serve, and no point naming a
 * running attempt to somebody who could not have typed into a finished one.
 * Reversing any pair would name a consequence where the reader needed the
 * cause.
 *
 * `disabled` is deliberately not derived here. The organization's
 * `workspaces.terminal.enabled` is a hub setting the client is never told, and
 * the honest client-side proxy for it is the capability: with the setting off
 * the hub refuses `requires: ["terminal"]` at creation, so no workspace ever
 * reports one. The sentence exists for the caller that does know — the
 * workspace whose request was refused carries the hub's own message.
 */
export function terminalBlockedReason(policy: TerminalPolicy): string | null {
  // 1. No issue, so no worktree can exist at all.
  if (policy.workItemId === null) return TERMINAL_POLICY_SENTENCES.noIssue;
  // 2. A viewer never gets a terminal, however generous the grant row is.
  if (policy.role === "viewer") return TERMINAL_POLICY_SENTENCES.viewer;
  // 3. `write` on the project.
  if (!policy.canWrite) return TERMINAL_POLICY_SENTENCES.noWriteGrant;
  // 4. The grant's `runners` flag, which is the terminal's own extra authority.
  if (!policy.canManageRunners) return TERMINAL_POLICY_SENTENCES.noRunnerGrant;
  // 5. A workspace that exists and is over. Unparaphrased, per §18.1.
  if (policy.workspaceState !== null && WORKSPACE_UNUSABLE_STATES.has(policy.workspaceState)) {
    return workspaceReasonSentence(policy.workspaceState, policy.workspaceReason ?? null);
  }
  // 6. A workspace on a running attempt refuses a terminal outright (§18.1).
  if (policy.workspaceReadOnly) return TERMINAL_POLICY_SENTENCES.attemptRunning;
  // 7. The request for a workspace failed outright. Already a sentence, and it
  //    is the hub's own — which is where "terminals are turned off for this
  //    organization" actually reaches a reader.
  if (policy.workspaceError !== null) return policy.workspaceError;
  // 8. A runner that reported no terminal. Null is not a refusal: it is what
  //    the panel looks like before anything has been asked for, and §18.1's
  //    `enabled` flag exists so that merely rendering the tab spends no slot.
  if (policy.terminalCapable === false) return TERMINAL_POLICY_SENTENCES.noCapability;
  return null;
}

/** Whether the launcher card, the menu item and the `T` shortcut are live. */
export function terminalAvailable(policy: TerminalPolicy): boolean {
  return terminalBlockedReason(policy) === null;
}
