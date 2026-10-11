// One Change Request: the round under review, its stored diff, the verdict,
// the reviews and the discussion, and the two decisions a reviewer makes.
//
// The hub addresses a change through its primary work item, so the page reads
// the issue, the project and the change detail together, and the diff for the
// selected round from the attempt that published it (§18.5). Approve binds to
// the current version — the hub refuses a stale approval with a 409 — and
// Request changes carries the review text the next run reads as its
// instructions. Neither moves the issue from here: that is the hub's rule for
// the decision, not this client's.
import { ArrowLeftIcon, GitPullRequestIcon } from "lucide-react";
import React from "react";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";

import { Button } from "../../components/ui/button.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import { toastManager } from "../../components/ui/toast.tsx";
import type {
  AttemptDiff,
  ChangeDetail,
  ChangeVersion,
  NativeIssue,
  NativeProject,
} from "../../contracts/work.ts";
import { useTheme } from "../adapters/theme.ts";
import { useShell, type ConnectionChip } from "../App.tsx";
import { useClient } from "../client.ts";
import { usePageTitle } from "../pageTitle.ts";
import { AttemptDiffBody } from "../components/surfaces/DiffSurface.tsx";
import { Pill, type PillTone } from "./components/IssueCard.tsx";
import { WorkTopBar } from "./components/WorkTopBar.tsx";
import { actorLabel } from "./lib/activity.ts";
import { currentVersion, useRoundDiff } from "./lib/roundDiff.ts";
import { ageLabel, issueNumber } from "./lib/format.ts";
import { NO_RUNNER_NAMES, useRunnerNames, type RunnerNames } from "./lib/runnerNames.ts";
import { useNow, useWorkHttp } from "./lib/useWork.ts";
import { newWorkKey, WorkApiError, type WorkHttp } from "./lib/workHttp.ts";

export { currentVersion, diffForRound } from "./lib/roundDiff.ts";

export type ReviewDecision = "approved" | "changes_requested";

/** The version the reader asked for, falling back to the current one. */
export function selectVersion(
  change: ChangeDetail,
  requested: string | null,
): ChangeVersion | null {
  if (requested !== null) {
    const named = change.versions.find((version) => version.version_id === requested);
    if (named !== undefined) return named;
  }
  return currentVersion(change);
}

/** The pill tone for the hub's rolled-up `summary.status`. */
export function statusTone(status: string): PillTone {
  switch (status) {
    case "reviewed":
    case "ready":
    case "merged":
    case "landed":
      return "ok";
    case "needs_evidence":
    case "pending":
      return "warn";
    case "stale_policy":
    case "blocked":
    case "failing":
      return "err";
    default:
      return "mute";
  }
}

function decisionTone(decision: string): PillTone {
  switch (decision) {
    case "approved":
      return "ok";
    case "changes_requested":
      return "err";
    default:
      return "mute";
  }
}

function decisionLabel(decision: string): string {
  switch (decision) {
    case "approved":
      return "approved";
    case "changes_requested":
      return "changes requested";
    default:
      return decision;
  }
}

/**
 * One sentence on where the round stands, from the summary the hub computed.
 * The messages under it are the hub's own, verbatim.
 */
export function statusSentence(change: ChangeDetail, version: ChangeVersion | null): string {
  const summary = change.summary;
  if (version === null) return "No version has been published yet, so there is nothing to review.";
  if (version.version_id !== change.change.current_version_id) {
    return `Round ${version.number} is not the current version. Review the current round to decide on it.`;
  }
  switch (summary.status) {
    case "reviewed":
      return summary.native_review === "not_required"
        ? "No review needed. The runner lands it on the base branch."
        : "Approved. Ready to land on the base branch.";
    case "landed":
      return "Landed on the base branch.";
    case "stale_policy":
      return "The project's policy changed after this round was published. A new round is needed.";
    case "needs_evidence":
      switch (summary.native_review) {
        case "changes_requested":
          return "Changes were requested. The next run picks the review up as its instructions.";
        case "approved":
          return "Approved, waiting on checks.";
        case "stale":
          return "An earlier round was approved. This round needs its own review.";
        default:
          return "Waiting for a review.";
      }
    case "draft":
      return "No version has been published yet, so there is nothing to review.";
    default:
      return summary.status.length === 0 ? "" : summary.status.replaceAll("_", " ");
  }
}

function short(sha: string | null | undefined): string {
  return sha === undefined || sha === null ? "" : sha.slice(0, 7);
}

function Row({ label, children }: { label: string; children: React.ReactNode }): React.ReactElement {
  return (
    <div className="grid grid-cols-[6.5rem_1fr] gap-x-3 py-0.5 text-xs">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 break-words">{children}</span>
    </div>
  );
}

function Mono({ children }: { children: React.ReactNode }): React.ReactElement {
  return <span className="font-mono text-[11px]">{children}</span>;
}

function Card({
  title,
  testId,
  children,
}: {
  title: string;
  testId: string;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <section className="rounded-lg border border-border bg-card p-3" data-testid={testId}>
      <h3 className="mb-1.5 font-medium text-sm">{title}</h3>
      {children}
    </section>
  );
}

export interface ChangeRequestViewProps {
  readonly issue: NativeIssue;
  readonly project: NativeProject;
  readonly change: ChangeDetail;
  readonly version: ChangeVersion | null;
  readonly diff: AttemptDiff | null;
  readonly diffLoading: boolean;
  /** A project write grant permits discussion comments. */
  readonly canWrite: boolean;
  /** Hosted reviews require an owner or admin with a project write grant. */
  readonly canReview: boolean;
  readonly busy: boolean;
  readonly now: number;
  readonly viewerPrincipalId: string;
  readonly runnerNames?: RunnerNames;
  readonly theme?: "light" | "dark";
  readonly onSelectVersion: (versionId: string) => void;
  readonly onReview: (decision: ReviewDecision, body: string) => Promise<void>;
  readonly onComment: (body: string) => Promise<void>;
}

/**
 * The page's body, pure in its inputs so the decisions can be checked
 * without a router or a hub behind them.
 */
export function ChangeRequestView(props: ChangeRequestViewProps): React.ReactElement {
  const { change, version, issue } = props;
  const [draft, setDraft] = React.useState("");
  const text = draft.trim();
  const current = version !== null && version.version_id === change.change.current_version_id;
  const decisions = props.canReview && version !== null;
  // No decision while the round's diff is still being read: what the reader
  // sees must be the round they decide on.
  const decisionDisabled = props.busy || props.diffLoading || !decisions;

  const act = async (run: () => Promise<void>) => {
    await run();
    setDraft("");
  };

  const reviews = change.reviews.filter(
    (review) => version === null || review.version_id === version.version_id,
  );
  const discussion = change.discussion.filter(
    (comment) =>
      comment.version_id === undefined ||
      version === null ||
      comment.version_id === version.version_id,
  );
  const round = (versionId: string): string => {
    const named = change.versions.find((candidate) => candidate.version_id === versionId);
    return named === undefined ? "" : `round ${named.number}`;
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden lg:flex-row">
      <div className="flex min-h-0 min-w-0 flex-1 flex-col" data-testid="change-diff">
        {props.diff === null ? (
          <div
            className="flex flex-1 items-center justify-center px-6 py-10 text-center text-muted-foreground text-sm"
            data-testid="change-diff-empty"
          >
            {props.diffLoading
              ? "Reading the diff…"
              : version === null
                ? "This change has no published version, so there is no diff to show."
                : "No stored diff is available for this round. The head and base are listed on the right."}
          </div>
        ) : (
          <AttemptDiffBody diff={props.diff} theme={props.theme ?? "light"} />
        )}
      </div>

      <aside
        className="flex w-full shrink-0 flex-col gap-3 overflow-y-auto border-border border-t p-3 lg:w-[22rem] lg:border-t-0 lg:border-l"
        data-testid="change-review"
      >
        <section
          className="rounded-lg border border-border bg-card p-3"
          data-testid="change-status-card"
        >
          <div className="mb-1.5 flex items-center gap-2">
            <Pill tone={statusTone(change.summary.status)} data-testid="change-status">
              {change.summary.status === "" ? "unknown" : change.summary.status.replaceAll("_", " ")}
            </Pill>
            <span className="text-muted-foreground text-xs">
              {issueNumber(`${props.project.name}#${issue.number}`, issue.number)} · {issue.state}
            </span>
          </div>
          <p className="text-xs" data-testid="change-status-sentence">
            {statusSentence(change, version)}
          </p>
          {change.summary.messages.length === 0 ? null : (
            <ul className="mt-2 list-disc space-y-1 ps-4 text-muted-foreground text-xs">
              {change.summary.messages.map((message) => (
                <li key={message}>{message}</li>
              ))}
            </ul>
          )}
        </section>

        {props.canWrite ? (
          <section
            className="rounded-lg border border-border bg-card p-3"
            data-testid="change-review-actions"
          >
            <h3 className="mb-1.5 font-medium text-sm">Your review</h3>
            <Textarea
              size="sm"
              aria-label="Review comment"
              placeholder="Say what needs to change, or leave a note with your approval."
              value={draft}
              disabled={props.busy}
              onChange={(event) => setDraft(event.currentTarget.value)}
            />
            <div className="mt-2 flex flex-wrap items-center gap-2">
              {decisions ? (
                <Button
                  size="sm"
                  data-testid="change-approve"
                  disabled={decisionDisabled || !current}
                  title={current ? undefined : "Only the current round can be approved"}
                  onClick={() => void act(() => props.onReview("approved", text))}
                >
                  Approve
                </Button>
              ) : null}
              {decisions ? (
                <Button
                  size="sm"
                  variant="destructive-outline"
                  data-testid="change-request-changes"
                  disabled={decisionDisabled || !current || text.length === 0}
                  title={
                    !current
                      ? "Only the current round can be sent back"
                      : text.length === 0
                        ? "Say what needs to change first"
                        : undefined
                  }
                  onClick={() => void act(() => props.onReview("changes_requested", text))}
                >
                  Request changes
                </Button>
              ) : null}
              <Button
                size="sm"
                variant="ghost"
                data-testid="change-comment"
                disabled={props.busy || props.diffLoading || text.length === 0}
                onClick={() => void act(() => props.onComment(text))}
              >
                Comment
              </Button>
            </div>
          </section>
        ) : null}

        <Card title="Rounds" testId="change-rounds">
          {change.versions.length === 0 ? (
            <p className="text-muted-foreground text-xs">No version published.</p>
          ) : (
            <ul className="space-y-1">
              {[...change.versions].reverse().map((candidate) => {
                const selected = version?.version_id === candidate.version_id;
                const isCurrent = candidate.version_id === change.change.current_version_id;
                return (
                  <li key={candidate.version_id}>
                    <button
                      type="button"
                      data-testid="change-round"
                      aria-pressed={selected}
                      onClick={() => props.onSelectVersion(candidate.version_id)}
                      className={`flex w-full items-center gap-2 rounded-md px-2 py-1 text-left text-xs outline-none ring-ring hover:bg-accent focus-visible:ring-2 ${
                        selected ? "bg-accent" : ""
                      }`}
                    >
                      <span className="font-medium">Round {candidate.number}</span>
                      <Mono>{short(candidate.head_sha)}</Mono>
                      {isCurrent ? <Pill tone="mute">current</Pill> : null}
                      <span className="ml-auto text-muted-foreground">
                        {ageLabel(candidate.created_at, props.now)}
                      </span>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </Card>

        {version === null ? null : (
          <Card title={`Round ${version.number}`} testId="change-round-card">
            <Row label="head">
              <Mono>{version.head_sha}</Mono>
            </Row>
            <Row label="base">
              <Mono>{version.base_sha}</Mono>
            </Row>
            <Row label="repository">
              <Mono>{version.repository}</Mono>
            </Row>
            <Row label="review">{change.summary.native_review.replaceAll("_", " ") || "—"}</Row>
            <Row label="checks">{change.summary.checks.replaceAll("_", " ") || "—"}</Row>
            {version.external === undefined ? null : (
              <Row label="pull request">
                <a
                  className="text-primary underline-offset-4 hover:underline"
                  href={version.external.url}
                  rel="noreferrer noopener"
                  target="_blank"
                >
                  #{version.external.id}
                </a>
              </Row>
            )}
          </Card>
        )}

        {change.checks.length === 0 ? null : (
          <Card title="Checks" testId="change-checks">
            <ul className="space-y-1">
              {change.checks.map((check) => (
                <li
                  key={check.check_run_id}
                  className="flex items-center gap-2 text-xs"
                  data-testid="change-check"
                >
                  <Pill tone={check.conclusion === "success" ? "ok" : "err"}>{check.conclusion}</Pill>
                  <Mono>{check.workflow_id}</Mono>
                  <span className="ml-auto text-muted-foreground">
                    {ageLabel(check.completed_at, props.now)}
                  </span>
                </li>
              ))}
            </ul>
          </Card>
        )}

        <Card title="Reviews" testId="change-reviews">
          {reviews.length === 0 ? (
            <p className="text-muted-foreground text-xs">No reviews on this round yet.</p>
          ) : (
            <ul className="space-y-2">
              {reviews.map((review) => (
                <li
                  key={review.review_id}
                  className="rounded-md border border-border p-2"
                  data-testid="change-review-entry"
                >
                  <p className="flex flex-wrap items-center gap-2 text-[11px] text-muted-foreground">
                    <Pill tone={decisionTone(review.decision)}>{decisionLabel(review.decision)}</Pill>
                    <span>
                      {actorLabel(
                        review.actor,
                        props.viewerPrincipalId,
                        null,
                        props.runnerNames ?? NO_RUNNER_NAMES,
                      )}
                    </span>
                    <span className="ml-auto">{ageLabel(review.created_at, props.now)}</span>
                  </p>
                  {review.body.length === 0 ? null : (
                    <p className="mt-1 whitespace-pre-wrap text-xs">{review.body}</p>
                  )}
                  {review.validator === undefined ? null : (
                    <section aria-label="Acceptance evidence" className="mt-3 space-y-2 text-xs">
                      <h3 className="font-medium">Acceptance evidence</h3>
                      {review.validator.criteria_evidence === undefined ? (
                        <p className="text-muted-foreground">No criterion evidence recorded for this review.</p>
                      ) : (
                        <ul className="space-y-2">
                          {review.validator.criteria_evidence.map((entry, index) => (
                            <li key={index} className="space-y-1">
                              <p className="flex flex-wrap items-center gap-2">
                                <Pill tone={entry.kind === "not_verified" ? "err" : "ok"}>
                                  {entry.kind === "not_verified" ? "Not verified" : entry.kind === "receipt" ? "Command receipt" : "Named test"}
                                </Pill>
                                <span className="whitespace-pre-wrap">{entry.criterion}</span>
                              </p>
                              {entry.reference === "" ? null : <p className="break-words text-muted-foreground">{entry.reference}</p>}
                              {entry.behavior === undefined ? null : <p className="whitespace-pre-wrap text-muted-foreground">{entry.behavior}</p>}
                            </li>
                          ))}
                        </ul>
                      )}
                      {(review.validator.not_verified?.length ?? 0) === 0 ? null : (
                        <section aria-label="Not verified criteria" className="space-y-1">
                          <h4 className="font-medium text-destructive">Not verified</h4>
                          <ul className="list-inside list-disc">
                            {review.validator.not_verified?.map((criterion, index) => <li key={index}>{criterion}</li>)}
                          </ul>
                        </section>
                      )}
                    </section>
                  )}
                </li>
              ))}
            </ul>
          )}
        </Card>

        {discussion.length === 0 ? null : (
          <Card title="Discussion" testId="change-discussion">
            <ul className="space-y-2">
              {discussion.map((comment) => (
                <li key={comment.comment_id} className="rounded-md border border-border p-2">
                  <p className="flex flex-wrap items-center gap-2 text-[11px] text-muted-foreground">
                    <span>
                      {actorLabel(
                        comment.actor,
                        props.viewerPrincipalId,
                        null,
                        props.runnerNames ?? NO_RUNNER_NAMES,
                      )}
                    </span>
                    {comment.version_id === undefined ? null : <span>{round(comment.version_id)}</span>}
                    <span className="ml-auto">{ageLabel(comment.created_at, props.now)}</span>
                  </p>
                  <p className="mt-1 whitespace-pre-wrap text-xs">{comment.body}</p>
                </li>
              ))}
            </ul>
          </Card>
        )}
      </aside>
    </div>
  );
}

interface PageData {
  readonly issue: NativeIssue;
  readonly project: NativeProject;
  readonly change: ChangeDetail;
}

function useChangeRequest(
  http: WorkHttp,
  projectId: string | null,
  workItemId: string,
  changeId: string,
): {
  data: PageData | null;
  error: string | null;
  loading: boolean;
  reload: () => void;
} {
  const [data, setData] = React.useState<PageData | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [nonce, setNonce] = React.useState(0);
  React.useEffect(() => {
    if (projectId === null) return;
    let cancelled = false;
    setLoading(true);
    void (async () => {
      try {
        const [issue, project, change] = await Promise.all([
          http.getWorkItem(projectId, workItemId),
          http.getProject(projectId),
          http.getChange(projectId, workItemId, changeId),
        ]);
        if (cancelled) return;
        setData({ issue, project, change });
        setError(null);
      } catch (cause) {
        if (cancelled) return;
        setError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [http, projectId, workItemId, changeId, nonce]);
  return { data, error, loading, reload: () => setNonce((value) => value + 1) };
}

export function ChangeRequestPage(): React.ReactElement {
  const { workItemId, changeId } = useParams({ from: "/work/i/$workItemId/changes/$changeId" });
  const search = useSearch({ strict: false }) as { project?: string };
  const shell = useShell();
  const navigate = useNavigate();
  const client = useClient();
  const http = useWorkHttp();
  const now = useNow();
  const { resolvedTheme } = useTheme();
  const runnerNames = useRunnerNames();

  // The project comes from the link when the row that opened this page knew
  // it (the all-projects Pull requests list), else from the linked
  // conversation, else from the shell's selected project.
  const linked = shell.conversations.find(
    (conversation) => conversation.work_item_id === workItemId,
  );
  const projectId =
    (search.project !== undefined && search.project.length > 0 ? search.project : null) ??
    linked?.project_id ??
    (shell.projectId === "" ? null : shell.projectId);
  const { data, error, loading, reload } = useChangeRequest(http, projectId, workItemId, changeId);
  const [requestedVersion, setRequestedVersion] = React.useState<string | null>(null);
  const version = data === null ? null : selectVersion(data.change, requestedVersion);
  const roundDiff = useRoundDiff(http, projectId, workItemId, version);
  const [busy, setBusy] = React.useState(false);

  const project =
    projectId === null
      ? null
      : (client.bootstrap.projects.find((candidate) => candidate.id === projectId) ?? null);
  usePageTitle(data === null ? "Change" : `Change: ${data.change.change.title}`, project?.name);
  const canWrite = project?.can_write === true;
  const canReview =
    canWrite && (client.bootstrap.actor.role === "owner" || client.bootstrap.actor.role === "admin");

  const openIssue = React.useCallback(
    () => void navigate({ to: "/work/i/$workItemId", params: { workItemId } }),
    [navigate, workItemId],
  );

  const failed = React.useCallback(
    (what: string, cause: unknown) => {
      const conflict = cause instanceof WorkApiError && cause.conflict;
      toastManager.add({
        type: conflict ? "warning" : "error",
        title: conflict ? "This change moved on while you were reading it" : `Could not ${what}`,
        description: conflict
          ? "A newer round was published. The page has been reloaded with what the hub has now."
          : cause instanceof Error
            ? cause.message
            : String(cause),
      });
      if (conflict) reload();
    },
    [reload],
  );

  const review = React.useCallback(
    async (decision: ReviewDecision, body: string) => {
      if (projectId === null || version === null) return;
      setBusy(true);
      try {
        await http.reviewChange({
          projectId,
          itemId: workItemId,
          changeId,
          versionId: version.version_id,
          key: newWorkKey("review"),
          decision,
          body,
          expectedVersionId: version.version_id,
        });
        toastManager.add({
          type: "success",
          title:
            decision === "approved"
              ? `Approved round ${version.number}`
              : `Changes requested on round ${version.number}`,
        });
        reload();
      } catch (cause) {
        failed(decision === "approved" ? "approve this round" : "request changes", cause);
        throw cause;
      } finally {
        setBusy(false);
      }
    },
    [changeId, failed, http, projectId, reload, version, workItemId],
  );

  const comment = React.useCallback(
    async (body: string) => {
      if (projectId === null) return;
      setBusy(true);
      try {
        await http.discussChange({
          projectId,
          itemId: workItemId,
          changeId,
          key: newWorkKey("discuss"),
          body,
          ...(version === null ? {} : { versionId: version.version_id }),
        });
        reload();
      } catch (cause) {
        failed("post the comment", cause);
        throw cause;
      } finally {
        setBusy(false);
      }
    },
    [changeId, failed, http, projectId, reload, version, workItemId],
  );

  const connection: ConnectionChip = loading
    ? { tone: "", label: "Loading", detail: "reading the change", action: null, tooltip: "Loading: reading the change request." }
    : error !== null
      ? { tone: "dc-err", label: "Unavailable", detail: null, action: { label: "Try again", onClick: reload }, tooltip: error }
      : {
          tone: "dc-ok",
          label: "Loaded",
          detail: data === null ? null : ageLabel(data.change.change.updated_at, now),
          action: { label: "Reload", onClick: reload },
          tooltip: "Loaded: the change as the hub last served it. Reload reads it again.",
        };

  const actions = (
    <Button size="xs" variant="ghost" onClick={openIssue} data-testid="change-open-issue">
      <ArrowLeftIcon className="size-3.5" />
      Issue
    </Button>
  );

  const body =
    projectId === null ? (
      <div className="flex min-h-0 flex-1 items-center justify-center p-8 text-muted-foreground text-sm">
        No project is selected for this change.
      </div>
    ) : data === null ? (
      <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-3 p-8 text-center text-muted-foreground text-sm">
        <p data-testid="change-page-status">{error ?? "Opening change…"}</p>
        {error === null ? null : (
          <Button size="sm" variant="outline" onClick={reload}>
            Try again
          </Button>
        )}
      </div>
    ) : (
      <ChangeRequestView
        issue={data.issue}
        project={data.project}
        change={data.change}
        version={version}
        diff={roundDiff.diff}
        diffLoading={roundDiff.loading}
        canWrite={canWrite}
        canReview={canReview}
        busy={busy}
        now={now}
        viewerPrincipalId={client.bootstrap.actor.principal_id}
        runnerNames={runnerNames}
        theme={resolvedTheme}
        onSelectVersion={setRequestedVersion}
        onReview={review}
        onComment={comment}
      />
    );

  return (
    <>
      <WorkTopBar
        context={data?.project.name ?? project?.name ?? "Work"}
        title={data?.change.change.title ?? "Change Request"}
        identifier={
          data === null
            ? null
            : issueNumber(`${data.project.name}#${data.issue.number}`, data.issue.number)
        }
        meta={
          version === null
            ? null
            : `Round ${version.number} · head ${short(version.head_sha)}`
        }
        connection={connection}
        actions={actions}
      />
      <span className="sr-only">
        <GitPullRequestIcon className="size-3" />
      </span>
      {body}
    </>
  );
}
