import React from "react";

import type { GitStackedAction, VcsStatusResult } from "../../contracts/ui.ts";
import { buildGitActionProgressStages } from "../../components/GitActionsControl.logic.ts";
import { getSourceControlPresentation } from "../../sourceControlPresentation.ts";
import {
  stackedThreadToast,
  toastManager,
  type ThreadToastData,
} from "../../components/ui/toast.tsx";
import { newWorkKey } from "../work/lib/workHttp.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import {
  gitGroupBlockedReason,
  toVcsStatus,
  type GitActionPolicy,
  type GitStatusPullRequest,
} from "./gitStatus.ts";
import {
  createWorkspaceRelay,
  RelayError,
  type GitCommittedPayload,
  type GitPushedPayload,
  type GitStatusPayload,
  type WorkspaceRelay,
} from "./workspaceRelay.ts";
import { isWorkspaceLive, useWorkspace } from "./workspaces.ts";

export interface GitOperations {
  status(): Promise<GitStatusPayload>;
  commit(message: string): Promise<GitCommittedPayload>;
  push(): Promise<GitPushedPayload>;
  createPullRequest(expectedHeadSha: string): Promise<void>;
}

export type GitSequenceStep = "status" | "commit" | "push" | "create-pr";

export interface GitSequenceResult {
  readonly ok: boolean;
  /** The step that failed, or null on success. */
  readonly failedAt: GitSequenceStep | null;
  readonly reason: string | null;
  /** Which steps actually ran, in order — a step with nothing to do is skipped. */
  readonly ran: readonly GitSequenceStep[];
  readonly excluded: readonly string[];
}

/** Which of the three mutating steps an action names, in order. */
function stepsFor(action: GitStackedAction): {
  commit: boolean;
  push: boolean;
  pullRequest: boolean;
} {
  switch (action) {
    case "commit":
      return { commit: true, push: false, pullRequest: false };
    case "push":
      return { commit: false, push: true, pullRequest: false };

    case "create_pr":
      return { commit: false, push: true, pullRequest: true };
    case "commit_push":
      return { commit: true, push: true, pullRequest: false };
    case "commit_push_pr":
      return { commit: true, push: true, pullRequest: true };
  }
}

/**
 * The sentence a failure gives the reader.
 *
 * A `git_failed` carries the tool's own standard error (§18.13), and that is
 * the only actionable thing about it — "Git refused the command" tells nobody
 * anything. The *last* non-empty line, because a push writes progress first
 * ("Enumerating objects…", "remote:" banners) and puts the reason last.
 */
export function gitFailureReason(cause: unknown): string {
  if (cause instanceof RelayError) {
    if (cause.stderr !== null) {
      const lines = cause.stderr
        .split("\n")
        .map((line) => line.trim())
        .filter((line) => line.length > 0);
      const last = lines.at(-1);
      if (last !== undefined) return last;
    }
    return cause.message;
  }
  return cause instanceof Error ? cause.message : String(cause);
}

export async function runGitSequence(input: {
  operations: GitOperations;
  action: GitStackedAction;
  message: string;
}): Promise<GitSequenceResult> {
  const ran: GitSequenceStep[] = [];
  let excluded: readonly string[] = [];
  const fail = (step: GitSequenceStep, reason: string): GitSequenceResult => ({
    ok: false,
    failedAt: step,
    reason,
    ran,
    excluded,
  });

  let status: GitStatusPayload;
  try {
    ran.push("status");
    status = await input.operations.status();
  } catch (cause) {
    return fail("status", gitFailureReason(cause));
  }

  const plan = stepsFor(input.action);

  if (plan.commit && status.dirty_file_count > 0) {
    try {
      ran.push("commit");
      const committed = await input.operations.commit(input.message);
      excluded = committed.excluded ?? [];
    } catch (cause) {
      return fail("commit", gitFailureReason(cause));
    }
    try {
      ran.push("status");
      status = await input.operations.status();
    } catch (cause) {
      return fail("status", gitFailureReason(cause));
    }
  }

  if (plan.push && !(status.upstream && status.ahead === 0)) {
    try {
      ran.push("push");
      await input.operations.push();
    } catch (cause) {
      return fail("push", gitFailureReason(cause));
    }
    try {
      ran.push("status");
      status = await input.operations.status();
    } catch (cause) {
      return fail("status", gitFailureReason(cause));
    }
  }

  if (plan.pullRequest) {
    if (status.head_sha === "") {
      // Better to refuse than to send an empty `expected_head_sha`: §18.6
      // checks the head against it, and an empty one is either rejected or,
      // worse, treated as "any head".
      return fail("create-pr", "The worktree reported no head commit to open against.");
    }
    try {
      ran.push("create-pr");
      await input.operations.createPullRequest(status.head_sha);
    } catch (cause) {
      return fail("create-pr", gitFailureReason(cause));
    }
  }

  return { ok: true, failedAt: null, reason: null, ran, excluded };
}

/**
 * What a finished run actually did, as one sentence.
 *
 * The steps rather than the action, because the action is what was asked for
 * and the steps are what happened: a `commit_push_pr` on a tree that went
 * clean between the label and the press commits nothing, and a reader told
 * "Committed, pushed" about a run that only opened a pull request would not
 * trust the next one.
 */
export function describeRun(result: GitSequenceResult, changeRequestLabel: string): string {
  const did: string[] = [];
  if (result.ran.includes("commit")) did.push("committed");
  if (result.ran.includes("push")) did.push("pushed");
  if (result.ran.includes("create-pr")) did.push(`opened a ${changeRequestLabel}`);
  const excluded =
    result.excluded.length === 0
      ? ""
      : ` ${result.excluded.length} denylisted ${
          result.excluded.length === 1 ? "path was" : "paths were"
        } left out.`;
  if (did.length === 0) return `Nothing to do; the worktree was already up to date.${excluded}`;
  const listed =
    did.length === 1
      ? did[0]
      : `${did.slice(0, -1).join(", ")} and ${did[did.length - 1] ?? ""}`;
  return `Detent ${listed}.${excluded}`;
}

// --- The hook ---------------------------------------------------------------

/** Both channels the group needs: files for §18.4's denylist, git for §18.13. */
const GIT_REQUIRES: readonly string[] = ["files", "git"];

export interface UseHeaderGitInput {
  readonly projectId: string | null;
  readonly workItemId: string | null;
  readonly canWrite: boolean;
  /** The project's GitHub connector, from §18.6's rows. Null when it has none. */
  readonly connector: { readonly baseUrl: string } | null;
  /** The issue's pull request, when it has one. */
  readonly pullRequest: GitStatusPullRequest | null;
  readonly threadToastData?: ThreadToastData | undefined;
}

export interface HeaderGit {
  /** What §18.6's policy layer answers from. */
  readonly policy: GitActionPolicy;

  readonly status: VcsStatusResult | null;

  readonly statusError: string | null;
  readonly busy: boolean;
  /** The absolute worktree path the runner reported, for `gitCwd`. */
  readonly worktreePath: string | null;
  /** The runner machine's hostname, for the Open picker's local/remote test. */
  readonly machineHostname: string | null;

  readonly run: (action: GitStackedAction, message: string) => void;

  readonly refresh: () => void;
}

export function useHeaderGit(input: UseHeaderGitInput): HeaderGit {
  const http = useWorkHttp();
  const { projectId, workItemId, canWrite, connector, pullRequest } = input;

  // §18.1's `enabled`: "False keeps the hook idle, so opening a panel is what
  // spends a slot." The group's first *interaction* is that moment — a press
  // on the primary or an open of the menu — never a render, because every
  // conversation with a linked issue renders this header.
  const [awake, setAwake] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [statusPayload, setStatusPayload] = React.useState<GitStatusPayload | null>(null);
  const [statusError, setStatusError] = React.useState<string | null>(null);

  const structurallyBlocked =
    workItemId === null ||
    !canWrite ||
    gitGroupBlockedReason({
      workItemId,
      canWrite,
      hasConnector: connector !== null,
      workspaceState: null,
      workspaceReadOnly: false,
      workspaceError: null,
      gitCapable: null,
      busy: false,
    }) !== null;

  const workspace = useWorkspace({
    projectId,
    workItemId,
    requires: GIT_REQUIRES,
    enabled: awake && !structurallyBlocked,
  });

  // One relay per live workspace, exactly as `RightPanel.tsx` opens the files
  // one: a new workspace id is a new socket, and the socket closes with it.
  const workspaceLive = workspace.state !== null && isWorkspaceLive(workspace.state);
  const relayUrl = workspace.relayUrl;
  const mintTicket = workspace.mintTicket;
  const relay = React.useMemo<WorkspaceRelay | null>(() => {
    if (!workspaceLive || relayUrl === null || mintTicket === null) return null;
    return createWorkspaceRelay({ url: relayUrl, mintTicket, channel: "git" });
  }, [workspaceLive, relayUrl, mintTicket]);
  React.useEffect(() => {
    if (relay === null) return;
    return () => relay.close();
  }, [relay]);

  const capabilities = workspace.workspace?.capabilities ?? null;
  const gitCapable = capabilities === null ? null : capabilities.git;

  const policy = React.useMemo<GitActionPolicy>(
    () => ({
      workItemId,
      canWrite,
      hasConnector: connector !== null,
      workspaceState: workspace.state,
      workspaceReadOnly: workspace.workspace?.read_only ?? false,
      workspaceError: workspace.error,
      gitCapable,
      busy,
      workspaceReason: workspace.reason,
    }),
    [
      busy,
      canWrite,
      connector,
      gitCapable,
      workItemId,
      workspace.error,
      workspace.reason,
      workspace.state,
      workspace.workspace?.read_only,
    ],
  );

  const status = React.useMemo<VcsStatusResult | null>(
    () =>
      statusPayload === null
        ? null
        : toVcsStatus({
            status: statusPayload,
            pullRequest,
            connector,
            // The one default branch this client ever knows: the base of the
            // pull request the issue already has (see `gitStatus.ts`).
            defaultBranch: pullRequest?.baseRef ?? null,
          }),
    [connector, pullRequest, statusPayload],
  );

  // What the reader asked for before the workspace was up. Readiness arrives
  // on the project event stream inside `useWorkspace` — nothing here polls —
  // and this effect is what turns that arrival into the run they asked for.
  const queued = React.useRef<{ action: GitStackedAction; message: string } | null>(null);
  const wantsStatus = React.useRef(false);

  const operationsFor = React.useCallback(
    (live: WorkspaceRelay, report: (step: GitSequenceStep) => void): GitOperations => ({
      status: async () => {
        report("status");
        const answer = await live.gitStatus();
        setStatusPayload(answer);
        setStatusError(null);
        return answer;
      },
      commit: async (message) => {
        report("commit");
        return live.gitCommit(message);
      },
      push: async () => {
        report("push");
        return live.gitPush();
      },
      createPullRequest: async (expectedHeadSha) => {
        report("create-pr");
        if (projectId === null || workItemId === null) {
          throw new Error("This conversation has no linked issue.");
        }
        await http.openPullRequest({
          projectId,
          itemId: workItemId,
          // One key per intent, as every other mutation in this client mints
          // one: a retry of the same press is the same request, and a second
          // press is a second intent.
          key: newWorkKey("pull-request-open"),
          expectedHeadSha,
        });
      },
    }),
    [http, projectId, workItemId],
  );

  const readStatus = React.useCallback(
    async (live: WorkspaceRelay) => {
      try {
        const answer = await live.gitStatus();
        setStatusPayload(answer);
        setStatusError(null);
      } catch (cause) {
        setStatusError(gitFailureReason(cause));
      }
    },
    [],
  );

  const execute = React.useCallback(
    async (live: WorkspaceRelay, action: GitStackedAction, message: string) => {
      const terminology = getSourceControlPresentation(status?.sourceControlProvider).terminology;
      const stages = buildGitActionProgressStages({
        action,
        hasCustomCommitMessage: message.trim().length > 0,
        hasWorkingTreeChanges: status?.hasWorkingTreeChanges ?? true,
        ...(statusPayload !== null && statusPayload.remote !== ""
          ? { pushTarget: statusPayload.remote }
          : {}),
        shouldPushBeforePr: action === "create_pr",
        terminology,
      });
      const scoped = input.threadToastData;
      const toastId = toastManager.add({
        type: "loading",
        title: stages[0] ?? "Running git action...",
        description: "Waiting for Git...",
        timeout: 0,
        ...(scoped !== undefined ? { data: scoped } : {}),
      });
      let stage = 0;
      const report = (step: GitSequenceStep) => {

        if (step === "status") return;
        stage += 1;
        const next = stages[Math.min(stage, stages.length - 1)];
        if (next === undefined) return;
        toastManager.update(toastId, {
          type: "loading",
          title: next,
          description: "Waiting for Git...",
          timeout: 0,
          ...(scoped !== undefined ? { data: scoped } : {}),
        });
      };
      setBusy(true);
      let result: GitSequenceResult;
      try {
        result = await runGitSequence({
          operations: operationsFor(live, report),
          action,
          message,
        });
      } finally {
        setBusy(false);
      }
      if (!result.ok) {
        toastManager.update(
          toastId,
          stackedThreadToast({
            type: "error",
            title: "Action failed",
            description: result.reason ?? "An error occurred.",
            ...(scoped !== undefined ? { data: scoped } : {}),
          }),
        );

        void readStatus(live);
        return;
      }
      toastManager.update(toastId, {
        type: "success",
        title: "Action complete",
        description: describeRun(result, terminology.shortLabel),
        timeout: 0,
        data: { ...(scoped ?? {}), dismissAfterVisibleMs: 10_000 },
      });
      void readStatus(live);
    },
    [input.threadToastData, operationsFor, readStatus, status, statusPayload],
  );

  React.useEffect(() => {
    if (relay === null) return;
    const pending = queued.current;
    if (pending !== null) {
      queued.current = null;
      wantsStatus.current = false;
      void execute(relay, pending.action, pending.message);
      return;
    }
    if (wantsStatus.current) {
      wantsStatus.current = false;
      void readStatus(relay);
    }
  }, [execute, readStatus, relay]);

  // A workspace that never comes up must not leave the group busy for ever:
  // the press is dropped and the reason is the workspace's own sentence, which
  // `gitGroupBlockedReason` then shows on the primary too.
  const workspaceError = workspace.error;
  React.useEffect(() => {
    if (workspaceError === null) return;
    if (queued.current === null && !wantsStatus.current) return;
    queued.current = null;
    wantsStatus.current = false;
    setBusy(false);
  }, [workspaceError]);

  const run = React.useCallback(
    (action: GitStackedAction, message: string) => {
      setAwake(true);
      if (relay === null) {
        // No workspace yet: remember the intent, ask for one, and let the
        // effect above run it when readiness arrives. Pressing is what spends
        // the slot, and the press is not lost while it is being spent.
        queued.current = { action, message };
        setBusy(true);
        return;
      }
      void execute(relay, action, message);
    },
    [execute, relay],
  );

  const refresh = React.useCallback(() => {
    setAwake(true);
    if (relay === null) {
      wantsStatus.current = true;
      return;
    }
    void readStatus(relay);
  }, [readStatus, relay]);

  return {
    policy,
    status,
    statusError,
    busy,
    worktreePath: workspace.workspace?.worktree_path ?? null,
    machineHostname: workspace.workspace?.machine_hostname ?? null,
    run,
    refresh,
  };
}
