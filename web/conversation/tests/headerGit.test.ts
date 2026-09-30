import { describe, expect, it } from "vitest";

import {
  describeRun,
  gitFailureReason,
  runGitSequence,
  type GitOperations,
} from "../src/app/adapters/headerGit.ts";
import {
  GIT_POLICY_SENTENCES,
  gitGroupBlockedReason,
  gitItemBlockedReason,
  toVcsStatus,
  type GitActionPolicy,
} from "../src/app/adapters/gitStatus.ts";
import { RelayError, type GitStatusPayload } from "../src/app/adapters/workspaceRelay.ts";
import type { GitStackedAction } from "../src/contracts/ui.ts";

function status(overrides: Partial<GitStatusPayload> = {}): GitStatusPayload {
  return {
    branch: "detent/acme_widgets_42",
    detached: false,
    remote: "origin",
    upstream: true,
    ahead: 0,
    behind: 0,
    dirty_file_count: 2,
    head_sha: "a".repeat(40),
    ...overrides,
  };
}

/**
 * Fakes for the four operations, with a script of status answers.
 *
 * A queue rather than one value, because the re-read between the steps is the
 * behaviour under test: a sequence that reused the first answer would look
 * identical until the commit changed something.
 */
function operations(input: {
  statuses: readonly GitStatusPayload[];
  onCommit?: () => Promise<{ commit: string; branch: string; files: number; excluded?: string[] }>;
  onPush?: () => Promise<void>;
  onCreate?: (sha: string) => Promise<void>;
}) {
  const calls: string[] = [];
  const shas: string[] = [];
  let read = 0;
  const ops: GitOperations = {
    status: async () => {
      calls.push("status");
      const answer = input.statuses[Math.min(read, input.statuses.length - 1)];
      read += 1;
      if (answer === undefined) throw new Error("no status scripted");
      return answer;
    },
    commit: async (message) => {
      calls.push(`commit:${message}`);
      if (input.onCommit !== undefined) return input.onCommit();
      return { commit: "b".repeat(40), branch: "detent/acme_widgets_42", files: 2 };
    },
    push: async () => {
      calls.push("push");
      if (input.onPush !== undefined) await input.onPush();
      return { branch: "detent/acme_widgets_42", remote: "origin", commit: "b".repeat(40) };
    },
    createPullRequest: async (sha) => {
      calls.push("create-pr");
      shas.push(sha);
      if (input.onCreate !== undefined) await input.onCreate(sha);
    },
  };
  return { ops, calls, shas };
}

describe("runGitSequence", () => {

  const dirtyAhead = status({ dirty_file_count: 2, ahead: 1, upstream: true });
  const cases: ReadonlyArray<{
    action: GitStackedAction;
    ran: readonly string[];
  }> = [
    { action: "commit", ran: ["status", "commit", "status"] },
    { action: "push", ran: ["status", "push", "status"] },
    { action: "create_pr", ran: ["status", "push", "status", "create-pr"] },
    { action: "commit_push", ran: ["status", "commit", "status", "push", "status"] },
    {
      action: "commit_push_pr",
      ran: ["status", "commit", "status", "push", "status", "create-pr"],
    },
  ];

  for (const testCase of cases) {
    it(`runs the steps ${testCase.action} names`, async () => {
      const { ops } = operations({
        statuses: [dirtyAhead, status({ dirty_file_count: 0, ahead: 2 })],
      });
      const result = await runGitSequence({ operations: ops, action: testCase.action, message: "m" });
      expect(result.ok).toBe(true);
      expect(result.failedAt).toBeNull();
      expect(result.ran).toEqual(testCase.ran);
    });
  }

  it("stops at the first failure and names the step and the reason", async () => {
    const { ops, calls } = operations({
      statuses: [dirtyAhead, status({ dirty_file_count: 0, ahead: 1 })],
      onPush: () => Promise.reject(new RelayError("read_only")),
    });
    const result = await runGitSequence({
      operations: ops,
      action: "commit_push_pr",
      message: "m",
    });
    expect(result.ok).toBe(false);
    expect(result.failedAt).toBe("push");
    expect(result.reason).toBe("This workspace is read-only.");
    // The pull request is never attempted after a failed push.
    expect(calls).not.toContain("create-pr");
  });

  it("skips a commit when nothing is dirty", async () => {
    const { ops } = operations({ statuses: [status({ dirty_file_count: 0, ahead: 1 })] });
    const result = await runGitSequence({ operations: ops, action: "commit_push", message: "m" });
    expect(result.ran).toEqual(["status", "push", "status"]);
    expect(result.ok).toBe(true);
  });

  it("skips a push when the branch is already in step with its upstream", async () => {
    const { ops } = operations({
      statuses: [status({ dirty_file_count: 0, upstream: true, ahead: 0 })],
    });
    const result = await runGitSequence({ operations: ops, action: "push", message: "m" });
    expect(result.ran).toEqual(["status"]);
    expect(result.ok).toBe(true);
  });

  it("pushes a branch with no upstream, however far ahead it is not", async () => {
    const { ops } = operations({
      statuses: [status({ dirty_file_count: 0, upstream: false, ahead: 0 })],
    });
    const result = await runGitSequence({ operations: ops, action: "push", message: "m" });
    expect(result.ran).toEqual(["status", "push", "status"]);
  });

  it("re-reads status between commit and push and opens the PR against the result", async () => {
    const { ops, shas, calls } = operations({
      statuses: [
        status({ dirty_file_count: 3, ahead: 0, head_sha: "a".repeat(40) }),
        status({ dirty_file_count: 0, ahead: 1, head_sha: "f".repeat(40) }),
      ],
      onCommit: async () => ({
        commit: "f".repeat(40),
        branch: "detent/acme_widgets_42",
        files: 3,
        excluded: [".env", "secrets/key.pem"],
      }),
    });
    const result = await runGitSequence({
      operations: ops,
      action: "commit_push_pr",
      message: "feat: header git group",
    });
    expect(calls).toContain("commit:feat: header git group");
    // The post-commit sha, not the one the button was labelled from.
    expect(shas).toEqual(["f".repeat(40)]);
    expect(result.excluded).toEqual([".env", "secrets/key.pem"]);
  });

  it("refuses a pull request with no head sha rather than sending an empty one", async () => {
    const { ops, shas } = operations({
      statuses: [status({ dirty_file_count: 0, upstream: true, ahead: 0, head_sha: "" })],
    });
    const result = await runGitSequence({ operations: ops, action: "create_pr", message: "" });
    expect(result.ok).toBe(false);
    expect(result.failedAt).toBe("create-pr");
    expect(result.reason).toBe("The worktree reported no head commit to open against.");
    expect(shas).toEqual([]);
  });

  it("surfaces the last non-empty line of a refused command's own output", async () => {
    const { ops } = operations({
      statuses: [status({ dirty_file_count: 0, ahead: 1 })],
      onPush: () =>
        Promise.reject(
          new RelayError(
            "git_failed",
            undefined,
            null,
            "Enumerating objects: 12, done.\nremote: Permission to acme/widgets.git denied.\n\n",
          ),
        ),
    });
    const result = await runGitSequence({ operations: ops, action: "push", message: "" });
    expect(result.reason).toBe("remote: Permission to acme/widgets.git denied.");
  });
});

describe("gitFailureReason", () => {
  it("falls back to the relay's own sentence when there is no output", () => {
    expect(gitFailureReason(new RelayError("stale_execution"))).toBe(
      "The runner lost its lease on this worktree.",
    );
  });

  it("reports a plain error and a non-error alike", () => {
    expect(gitFailureReason(new Error("boom"))).toBe("boom");
    expect(gitFailureReason("boom")).toBe("boom");
  });
});

describe("describeRun", () => {
  it("names the steps that ran, not the action that was asked for", () => {
    expect(
      describeRun(
        { ok: true, failedAt: null, reason: null, ran: ["status", "push", "status"], excluded: [] },
        "PR",
      ),
    ).toBe("Detent pushed.");
    expect(
      describeRun(
        {
          ok: true,
          failedAt: null,
          reason: null,
          ran: ["status", "commit", "status", "push", "status", "create-pr"],
          excluded: [".env"],
        },
        "PR",
      ),
    ).toBe("Detent committed, pushed and opened a PR. 1 denylisted path was left out.");
  });

  it("says so when there was nothing to do", () => {
    expect(
      describeRun({ ok: true, failedAt: null, reason: null, ran: ["status"], excluded: [] }, "PR"),
    ).toBe("Nothing to do; the worktree was already up to date.");
  });
});

function policy(overrides: Partial<GitActionPolicy> = {}): GitActionPolicy {
  return {
    workItemId: "wi_1",
    canWrite: true,
    hasConnector: true,
    workspaceState: "ready",
    workspaceReadOnly: false,
    workspaceError: null,
    gitCapable: true,
    busy: false,
    ...overrides,
  };
}

describe("the §18.6 policy layer", () => {
  const table: ReadonlyArray<{
    name: string;
    policy: GitActionPolicy;
    expected: string | null;
  }> = [
    { name: "everything in place", policy: policy(), expected: null },
    {
      name: "no linked issue",
      policy: policy({ workItemId: null }),
      expected: GIT_POLICY_SENTENCES.noIssue,
    },
    {
      name: "no write grant, even with an issue",
      policy: policy({ canWrite: false }),
      expected: GIT_POLICY_SENTENCES.noWriteGrant,
    },
    {
      name: "a failed workspace, in §18.1's own words",
      policy: policy({ workspaceState: "failed", workspaceReason: "checkout_failed" }),
      expected: "The runner could not check the repository out.",
    },
    {
      name: "a closed workspace",
      policy: policy({ workspaceState: "closed", workspaceReason: "expired" }),
      expected: "This workspace reached its idle timeout and closed.",
    },
    {
      name: "a read-only workspace because the attempt is still running",
      policy: policy({ workspaceReadOnly: true }),
      expected: GIT_POLICY_SENTENCES.readOnly,
    },
    {
      name: "a request for a workspace that failed outright",
      policy: policy({ workspaceError: "You have 3 workspaces open, which is the limit." }),
      expected: "You have 3 workspaces open, which is the limit.",
    },
    {
      name: "a runner that does not serve version control",
      policy: policy({ gitCapable: false }),
      expected: GIT_POLICY_SENTENCES.noGitCapability,
    },
    {
      name: "a workspace still opening",
      policy: policy({ workspaceState: "starting", gitCapable: null }),
      expected: GIT_POLICY_SENTENCES.opening,
    },

    { name: "busy", policy: policy({ busy: true, workspaceState: "starting" }), expected: null },
    {
      name: "nothing asked for yet",
      policy: policy({ workspaceState: null, gitCapable: null }),
      expected: null,
    },
  ];

  for (const row of table) {
    it(`answers ${row.name}`, () => {
      expect(gitGroupBlockedReason(row.policy)).toBe(row.expected);
    });
  }

  it("never says a status has not been read; that sentence is shared UI's", () => {
    expect(gitGroupBlockedReason(policy({ gitCapable: null }))).not.toBe(
      GIT_POLICY_SENTENCES.statusUnread,
    );
  });

  it("blocks only the pull request row on a project with no connector", () => {
    const withoutConnector = policy({ hasConnector: false });
    expect(gitItemBlockedReason("commit", withoutConnector)).toBeNull();
    expect(gitItemBlockedReason("push", withoutConnector)).toBeNull();
    expect(gitItemBlockedReason("pr", withoutConnector)).toBe("No GitHub connector on this project.");
  });

  it("prefers the group's structural reason to the row's own", () => {
    expect(gitItemBlockedReason("pr", policy({ workItemId: null, hasConnector: false }))).toBe(
      GIT_POLICY_SENTENCES.noIssue,
    );
  });
});

describe("toVcsStatus", () => {
  it("maps the status frame onto the fields the table branches on", () => {
    const projected = toVcsStatus({
      status: status({ dirty_file_count: 4, ahead: 3, behind: 1, upstream: true }),
      pullRequest: null,
      connector: { baseUrl: "https://github.com" },
    });
    expect(projected).toMatchObject({
      isRepo: true,
      refName: "detent/acme_widgets_42",
      hasWorkingTreeChanges: true,
      hasUpstream: true,
      aheadCount: 3,
      behindCount: 1,
      hasPrimaryRemote: true,
      isDefaultRef: false,
      pr: null,
      sourceControlProvider: { kind: "github", name: "GitHub", baseUrl: "https://github.com" },
    });
    // §18.13's status frame carries a count and no file list, deliberately.
    expect(projected.workingTree).toEqual({ files: [], insertions: 0, deletions: 0 });
  });

  it("reports a detached head as no ref, and no remote as no primary remote", () => {
    const projected = toVcsStatus({
      status: status({ detached: true, remote: "", dirty_file_count: 0 }),
      pullRequest: null,
      connector: null,
    });
    expect(projected.refName).toBeNull();
    expect(projected.hasPrimaryRemote).toBe(false);
    expect(projected.hasWorkingTreeChanges).toBe(false);
    expect(projected.sourceControlProvider).toBeUndefined();
  });

  it("carries the §18.6 pull request through the projection", () => {
    const projected = toVcsStatus({
      status: status(),
      pullRequest: {
        number: 1157,
        title: "feat(header): the git group",
        url: "https://github.com/acme/widgets/pull/1157",
        baseRef: "main",
        headRef: "detent/acme_widgets_42",
        state: "open",
        isDraft: true,
        updatedAt: "2026-09-12T08:00:00Z",
      },
      connector: { baseUrl: "https://github.com" },
    });
    expect(projected.pr).toMatchObject({
      number: 1157,
      baseRef: "main",
      headRef: "detent/acme_widgets_42",
      state: "open",
      isDraft: true,
    });
  });

  it("resolves isDefaultRef only when it actually knows the default branch", () => {
    expect(
      toVcsStatus({
        status: status({ branch: "main" }),
        pullRequest: null,
        connector: null,
        defaultBranch: "main",
      }).isDefaultRef,
    ).toBe(true);
    expect(
      toVcsStatus({ status: status({ branch: "main" }), pullRequest: null, connector: null })
        .isDefaultRef,
    ).toBe(false);
  });
});

