import {
  WORKSPACE_UNUSABLE_STATES,
  type WorkspaceReason,
  type WorkspaceState,
} from "../../contracts/work.ts";
import type {
  PullRequestState,
  SourceControlProviderInfo,
  VcsStatusResult,
} from "../../contracts/ui.ts";
import type { GitStatusPayload } from "./workspaceRelay.ts";
import { pullRequestDetailToVcsStatus } from "./pullRequestVcs.ts";
import { workspaceReasonSentence } from "./workspaces.ts";

/** The §18.6 row as this projection needs it. `null` means the issue has none. */
export interface GitStatusPullRequest {
  readonly number: number;
  readonly title: string;
  readonly url: string;
  readonly baseRef: string;
  readonly headRef: string;
  readonly state: PullRequestState;
  readonly isDraft: boolean;
  readonly updatedAt: string;
}

export interface ToVcsStatusInput {
  readonly status: GitStatusPayload;
  readonly pullRequest: GitStatusPullRequest | null;

  readonly connector: { readonly baseUrl: string } | null;

  readonly defaultBranch?: string | null | undefined;
}

export function toVcsStatus(input: ToVcsStatusInput): VcsStatusResult {
  const { status } = input;
  const refName = status.detached ? null : status.branch;
  const provider: SourceControlProviderInfo | undefined =
    input.connector === null
      ? undefined
      : { kind: "github", name: "GitHub", baseUrl: input.connector.baseUrl };
  const defaultBranch = input.defaultBranch ?? null;
  return {
    // The channel only answers at all for a worktree that is a repository:
    // §18.13 fails a request against anything else with `unsupported`, so a
    // status frame in hand is itself the proof.
    isRepo: true,
    ...(provider === undefined ? {} : { sourceControlProvider: provider }),
    hasPrimaryRemote: status.remote !== "",
    isDefaultRef: refName !== null && defaultBranch !== null && refName === defaultBranch,
    refName,
    hasWorkingTreeChanges: status.dirty_file_count > 0,
    workingTree: { files: [], insertions: 0, deletions: 0 },
    hasUpstream: status.upstream,
    aheadCount: status.ahead,
    behindCount: status.behind,
    pr:
      input.pullRequest === null
        ? null
        : pullRequestDetailToVcsStatus({
            number: input.pullRequest.number,
            title: input.pullRequest.title,
            url: input.pullRequest.url,
            baseBranch: input.pullRequest.baseRef,
            headBranch: input.pullRequest.headRef,
            state: input.pullRequest.state,
            ...(input.pullRequest.isDraft ? { isDraft: true } : {}),
            updatedAt: input.pullRequest.updatedAt,
            provider: "github",
          }),
  };
}

// --- The §18.6 policy layer -------------------------------------------------

export interface GitActionPolicy {
  readonly workItemId: string | null;
  readonly canWrite: boolean;
  readonly hasConnector: boolean;
  readonly workspaceState: WorkspaceState | null;
  readonly workspaceReadOnly: boolean;
  readonly workspaceError: string | null;
  /** null until the workspace reports its capabilities. */
  readonly gitCapable: boolean | null;
  readonly busy: boolean;
  /** The `reason` a failed or closed workspace carried, where it carried one. */
  readonly workspaceReason?: WorkspaceReason | null | undefined;
}

/**
 * Every sentence this layer can say, in one place.
 *
 * One sentence each, ending in a full stop, in the voice of the rest of the
 * client: name the fact, not the fix, unless the fix is one step.
 */
export const GIT_POLICY_SENTENCES = {
  noIssue: "Link an issue to get a worktree.",
  noWriteGrant: "You need write access on this project to commit or push.",
  readOnly: "This worktree is read-only while the attempt is still running.",
  opening: "This worktree is still opening.",
  noGitCapability: "This runner does not serve version control for its worktrees.",
  noConnector: "No GitHub connector on this project.",

  statusUnread: "The worktree's status has not been read yet.",
} as const;

export function gitGroupBlockedReason(policy: GitActionPolicy): string | null {
  // 1. No issue, so no worktree can exist at all.
  if (policy.workItemId === null) return GIT_POLICY_SENTENCES.noIssue;
  // 2. No grant, so no worktree may be written to.
  if (!policy.canWrite) return GIT_POLICY_SENTENCES.noWriteGrant;
  // 3. A workspace that exists and is over. Unparaphrased, per §18.1.
  if (policy.workspaceState !== null && WORKSPACE_UNUSABLE_STATES.has(policy.workspaceState)) {
    return workspaceReasonSentence(policy.workspaceState, policy.workspaceReason ?? null);
  }
  // 4. A workspace that exists and refuses writes: §18.13 answers a write on
  //    one whose attempt is still running with `read_only`.
  if (policy.workspaceReadOnly) return GIT_POLICY_SENTENCES.readOnly;
  // 5. The request for a workspace failed outright. Already a sentence.
  if (policy.workspaceError !== null) return policy.workspaceError;
  // 6. A runner that claimed the workspace without the git capability.
  if (policy.gitCapable === false) return GIT_POLICY_SENTENCES.noGitCapability;

  if (policy.busy) return null;
  // 8. Least structural: a workspace on its way. Only reachable when nothing
  //    is in flight, because an action the reader started keeps `busy` true
  //    through the wait for readiness.
  if (policy.workspaceState !== null && !isReadyEnough(policy.workspaceState)) {
    return GIT_POLICY_SENTENCES.opening;
  }
  return null;
}

export function gitItemBlockedReason(
  id: "commit" | "push" | "pr",
  policy: GitActionPolicy,
): string | null {
  // A group reason blocks every item in it, and is more structural than
  // anything an individual item can say about itself.
  const group = gitGroupBlockedReason(policy);
  if (group !== null) return group;

  if (id === "pr" && !policy.hasConnector) return GIT_POLICY_SENTENCES.noConnector;
  return null;
}

/** A workspace the relay can carry frames for (§18.1's `ready` and `idle`). */
function isReadyEnough(state: WorkspaceState): boolean {
  return state === "ready" || state === "idle";
}
