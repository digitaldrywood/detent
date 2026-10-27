import * as Schema from "effect/Schema";
import { NativeAttempt as NativeAttemptSchema } from "../../../contracts/work.ts";
import { ArchiveUnavailable } from "../../../runtime/archiveDecode.ts";
// The issue page's read, shared by the page and by link prefetch.
import type {
  ChangeDetail,
  ChangeRequestList,
  CollaborationEvent,
  NativeAttempt,
  NativeComment,
  NativeIssue,
  NativeProject,
} from "../../../contracts/work.ts";
import { getQueryData, prefetchQuery, scopeOf, type Read } from "../../lib/queryCache.ts";
import { WorkApiError, type WorkHttp } from "./workHttp.ts";

export interface IssueData {
  readonly attemptsCursor?: string | null;
  readonly historyCursor?: string | null;
  readonly commentsCursor?: string | null;
  readonly historyCompacted?: boolean;
  readonly attemptsAvailable?: boolean;
  readonly historyAvailable?: boolean;
  readonly issue: NativeIssue;
  readonly project: NativeProject;
  readonly attempts: readonly NativeAttempt[];
  readonly history: readonly CollaborationEvent[];
  readonly comments: readonly NativeComment[];
  readonly change: ChangeDetail | null;
  readonly changes: readonly {
    readonly record: { readonly change_id: string; readonly title: string };
    readonly detail: ChangeDetail | null;
    /** The detail is still loading after the first paint. */
    readonly pending?: boolean;
    readonly deferred?: boolean;
  }[];
}

/**
 * The change the page opens: the one the link named when the issue has it,
 * otherwise the latest. A link to a specific change (the hub's old
 * `/projects/:project/issues/:item/changes/:change` page) keeps pointing at
 * that change rather than whatever was published after it.
 */
export function selectChangeId(
  changes: readonly { readonly change_id: string }[],
  requested: string | null | undefined,
): string | null {
  if (requested !== undefined && requested !== null && requested.length > 0) {
    if (changes.some((change) => change.change_id === requested)) return requested;
  }
  return changes.at(-1)?.change_id ?? null;
}

/** The cache key for one issue; `["issue", id]` addresses every view of it. */
export function issueKey(http: WorkHttp, workItemId: string, requestedChange: string | null) {
  return ["issue", workItemId, scopeOf(http), requestedChange] as const;
}

const projectHints = new Map<string, string>();

/** Records the project a work item belongs to, so its page can skip a round trip. */
export function hintIssueProject(workItemId: string, projectId: string): void {
  projectHints.set(workItemId, projectId);
  if (projectHints.size > 2000) projectHints.delete(projectHints.keys().next().value!);
}

export function issueProjectHint(http: WorkHttp, workItemId: string): string | undefined {
  return getQueryData<IssueData | null>(issueKey(http, workItemId, null))?.issue.project_id ?? projectHints.get(workItemId);
}

/** The cache key for an issue's head: the item and its project, all a hover prefetches. */
function headKey(http: WorkHttp, workItemId: string) {
  return ["issue-head", workItemId, scopeOf(http)] as const;
}

interface IssueHead {
  readonly issue: NativeIssue;
  readonly project: NativeProject;
}

function related(http: WorkHttp, projectId: string, workItemId: string, signal: AbortSignal, project?: NativeProject, issue?: NativeIssue) {
  return Promise.all([
    project?.project_id === projectId ? project : http.getProject(projectId, signal),
    issue?.archive_summary !== undefined ? Promise.resolve({ items: Schema.decodeUnknownSync(Schema.Array(NativeAttemptSchema))(issue.archive_summary.attempts), next_cursor: null }) : http.listAttempts(projectId, workItemId, 100, signal, undefined, "newest").catch((error: unknown) => { if (error instanceof ArchiveUnavailable) throw error; return null; }),
    http.listHistory({ projectId, itemId: workItemId, limit: 100, direction: "newest", view: "timeline", signal }).catch(() => null),
    http.listComments({ projectId, itemId: workItemId, limit: 100, direction: "newest", signal }).catch(() => ({ items: [] as readonly NativeComment[], next_cursor: null })),
    http.listChanges(projectId, workItemId).catch(() => [] as ChangeRequestList),
  ]);
}

export function readIssue(http: WorkHttp, workItemId: string, requestedChange: string | null, refreshProject = false, snapshot = false): Read<IssueData | null> {
  return async (signal, partial) => {
    try {
      const cached = getQueryData<IssueData | null>(issueKey(http, workItemId, requestedChange));
      const head = getQueryData<IssueHead>(headKey(http, workItemId));
      const known = cached?.issue.project_id ?? head?.project.project_id ?? projectHints.get(workItemId);
      const knownProject = refreshProject ? undefined : cached?.project ?? head?.project;
      let issue: NativeIssue;
      let rest: Awaited<ReturnType<typeof related>>;
      if (known === undefined || snapshot) {
        issue = await http.getWorkItemById(workItemId, signal);
        rest = await related(http, issue.project_id, workItemId, signal,
          issue.project_id === known ? knownProject : undefined, issue);
      } else {
        const [resolved, speculative] = await Promise.all([
          http.getWorkItemById(workItemId, signal),
          related(http, known, workItemId, signal, knownProject, cached?.issue ?? head?.issue).then((value) => ({ value }), (error: unknown) => ({ error })),
        ]);
        issue = resolved;
        if (issue.project_id !== known) rest = await related(http, issue.project_id, workItemId, signal, undefined, issue);
        else if ("error" in speculative) throw speculative.error;
        else rest = speculative.value;
      }
      const projectId = issue.project_id;
      hintIssueProject(workItemId, projectId);
      const [project, attempts, history, comments, changes] = rest;
      const first: IssueData = {
        issue, project, attempts: [...(attempts?.items ?? [])].sort((a, b) => Number(BigInt(a.fencing_token) - BigInt(b.fencing_token))),
        history: [...(history?.items ?? [])].sort((a, b) => Number(BigInt(a.aggregate_sequence) - BigInt(b.aggregate_sequence))),
        comments: comments.items, attemptsCursor: attempts?.next_cursor, historyCursor: history?.next_cursor, commentsCursor: comments.next_cursor,
        historyCompacted: (history?.compaction?.removed_events ?? 0) > 0,
        attemptsAvailable: attempts !== null, historyAvailable: history !== null, change: null, changes: changes.map((record) => ({ record, detail: null, pending: true })),
      };
      if (changes.length === 0 || issue.archive_summary !== undefined && requestedChange === null) return { ...first, changes: first.changes.map((entry) => ({ ...entry, pending: false, deferred: true })) };
      if (cached === undefined || cached === null) partial(first);
      const details = await Promise.all(
        changes.map(async (record) => ({
          record,
          detail: await http.getChange(projectId, workItemId, record.change_id).catch((error: unknown) => { if (error instanceof ArchiveUnavailable) throw error; return null; }),
        })),
      );
      const selected = selectChangeId(changes, requestedChange);
      const current = getQueryData<IssueData | null>(issueKey(http, workItemId, requestedChange));
      return { ...(current?.issue === first.issue ? current : first), change: details.find((entry) => entry.record.change_id === selected)?.detail ?? null, changes: details };
    } catch (cause) {
      if (cause instanceof WorkApiError && [401, 403, 404].includes(cause.status)) partial(null);
      throw cause;
    }
  };
}

/**
 * Starts an issue's first-paint head (the item and its project) for a reader
 * about to open it. `signal` cancels it when the pointer leaves first.
 */
export function prefetchIssue(http: WorkHttp, workItemId: string, projectId?: string, signal?: AbortSignal): void {
  if (projectId !== undefined) hintIssueProject(workItemId, projectId);
  prefetchQuery(headKey(http, workItemId), async (read) => {
    const [issue, project] = await Promise.all([
      http.getWorkItemById(workItemId, read),
      projectId === undefined ? undefined : http.getProject(projectId, read),
    ]);
    return { issue, project: project?.project_id === issue.project_id ? project : await http.getProject(issue.project_id, read) };
  }, 10_000, signal);
}

export async function readOlderIssue(http: WorkHttp, data: IssueData): Promise<IssueData> {
  const projectId = data.issue.project_id;
  const itemId = data.issue.work_item_id;
  const [attempts, history, comments] = await Promise.all([
    data.attemptsCursor ? http.listAttempts(projectId, itemId, 100, undefined, data.attemptsCursor, "newest") : null,
    data.historyCursor ? http.listHistory({ projectId, itemId, limit: 100, cursor: data.historyCursor, direction: "newest", view: "timeline" }) : null,
    data.commentsCursor ? http.listComments({ projectId, itemId, limit: 100, cursor: data.commentsCursor, direction: "newest" }) : null,
  ]);
  for (const [page, cursor] of [[attempts, data.attemptsCursor], [history, data.historyCursor], [comments, data.commentsCursor]] as const) {
    if (page?.next_cursor && page.next_cursor === cursor) throw new Error("Issue history cursor did not advance");
  }
  return {
    ...data,
    attempts: [...(attempts?.items ?? []).filter((entry) => !data.attempts.some((current) => current.attempt_id === entry.attempt_id)), ...data.attempts].sort((a, b) => Number(BigInt(a.fencing_token) - BigInt(b.fencing_token))),
    history: [...(history?.items ?? []).filter((entry) => !data.history.some((current) => current.event_id === entry.event_id)), ...data.history].sort((a, b) => Number(BigInt(a.aggregate_sequence) - BigInt(b.aggregate_sequence))),
    comments: [...(comments?.items ?? []).filter((entry) => !data.comments.some((current) => current.comment_id === entry.comment_id)), ...data.comments],
    attemptsCursor: attempts === null ? data.attemptsCursor : attempts.next_cursor,
    historyCursor: history === null ? data.historyCursor : history.next_cursor,
    commentsCursor: comments === null ? data.commentsCursor : comments.next_cursor,
  };
}
