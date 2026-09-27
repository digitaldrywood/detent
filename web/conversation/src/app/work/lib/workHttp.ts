// Same-origin HTTP for the hub's native work API.
//
// A sibling of `runtime/rpc/http.ts` rather than an extension of it: that
// client is the conversation transport the Effect supervisor owns, and the
// board's reads are ordinary request/response calls with no session, no
// cursor bookkeeping and no stream to keep alive. They are plain promises so
// a component can `await` one, and every response is decoded through the
// schemas in `contracts/work.ts` so a shape change fails here rather than
// three renders later.
//
// Authentication is the same as the conversation client's: the session cookie
// travels with `credentials: "same-origin"`, and every mutation carries the
// `X-CSRF-Token` from the bootstrap payload, which the hosted boundary
// requires of any non-GET without an `Authorization` header.
import * as Schema from "effect/Schema";

import {
  Action,
  ActionList,
  ActionRun,
  ActionRunAccepted,
  ActionRunList,
  AttemptPage,
  ChangeDetail,
  ChangeRequestList,
  CommentPage,
  HistoryPage,
  NativeComment,
  NativeError,
  NativeIssue,
  NativeLabelList,
  NativeProject,
  PullRequestActionAccepted,
  RelayTicket,
  REVISION_CONFLICT,
  type TransitionReason,
  WorkItemPage,
  WorkItemPullRequestList,

  Workspace,
  WorkspaceList,
  WorkItemDiff,
} from "../../../contracts/work.ts";
import { hubPath } from "../../../runtime/basePath.ts";

/**
 * A failed request.
 *
 * `currentRevision` is present only where the hub sent one. In hosted mode it
 * never does — `nativeAPIError` rebuilds the body without it — so a conflict
 * handler must re-read the work item rather than trusting this field to be
 * there. `conflict` is the one distinction the board acts on.
 */
export class WorkApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly currentRevision: string | null;
  readonly details: Record<string, unknown> | null;

  constructor(input: {
    status: number;
    code: string;
    message: string;
    currentRevision?: string | null;
    details?: Record<string, unknown> | null;
  }) {
    super(input.message);
    this.name = "WorkApiError";
    this.status = input.status;
    this.code = input.code;
    this.currentRevision = input.currentRevision ?? null;
    this.details = input.details ?? null;
  }

  /** A lost update: someone else moved or edited this item first. */
  get conflict(): boolean {
    return this.status === 409 && this.code === REVISION_CONFLICT;
  }
}

function messageForStatus(status: number): string {
  if (status === 401) return "Your session has expired. Sign in again.";
  if (status === 403) return "You do not have permission to do that.";
  if (status === 404) return "That work item is not available.";
  if (status === 409) return "Someone else changed this first.";
  if (status >= 500) return "The hub could not complete the request.";
  return `The request failed (${status}).`;
}

const decodeError = Schema.decodeUnknownSync(NativeError);

function errorFromBody(status: number, body: unknown): WorkApiError {
  try {
    const parsed = decodeError(body);
    return new WorkApiError({
      status,
      code: parsed.code,
      message: parsed.message.length > 0 ? parsed.message : messageForStatus(status),
      currentRevision: parsed.current_revision ?? null,
      details: (parsed.details as Record<string, unknown> | undefined) ?? null,
    });
  } catch {
    // Echo's own 404 and 405 are `{"message": "Not Found"}` with no code, and
    // the hosted event stream answers with no body at all.
    return new WorkApiError({
      status,
      code: status === 404 ? "not_found" : "invalid",
      message: messageForStatus(status),
    });
  }
}

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface WorkHttpOptions {
  /** Absolute or relative origin. Empty for same-origin. */
  readonly origin: string;
  /** `api_base` from the bootstrap payload. */
  readonly apiBase: string;
  readonly csrfToken: string;
  readonly fetch?: FetchLike;
}

/**
 * The hub's list filters are strictly single-valued: `validateNativeQuery`
 * rejects a repeated parameter with `422`. A multi-select filter therefore
 * pushes exactly one value to the server and the rest are applied to what came
 * back; `serverFilter` is where that decision is made, in one place, so no
 * call site can accidentally send two.
 */
export function serverFilter(values: readonly string[]): string | undefined {
  return values.length === 1 ? values[0] : undefined;
}

export interface ListWorkItemsInput {
  readonly projectId: string;
  readonly state?: string | undefined;
  readonly label?: string | undefined;
  readonly assignee?: string | undefined;
  readonly priority?: string | undefined;
  readonly cursor?: string | undefined;
  readonly limit?: number | undefined;
  /** Coordinator work items are excluded unless this is on. */
  readonly includeCoordinator?: boolean;
}

export interface WorkHttp {
  readonly origin: string;
  readonly apiBase: string;
  /**
   * The hosted activity stream. It is a page route, not an API route, and it
   * emits `event: activity` with a bare decimal sequence as its data.
   */
  readonly eventsUrl: (projectId: string) => string;
  readonly getProject: (projectId: string) => Promise<NativeProject>;
  readonly listWorkItems: (input: ListWorkItemsInput) => Promise<WorkItemPage>;
  readonly getWorkItem: (projectId: string, itemId: string) => Promise<NativeIssue>;
  readonly patchWorkItem: (input: {
    projectId: string;
    itemId: string;
    key: string;
    expectedRevision: string;
    title?: string;
    body?: string;
    /** A number sets the level; `"none"` removes the priority entirely. */
    priority?: number | "none";
    labels?: readonly string[];
    assignees?: readonly string[];
  }) => Promise<NativeIssue>;
  /**
   * The project's label catalogue. The hub has no label table, so this is the
   * union of what the project's work items carry, each with a colour derived
   * from its name — which is why a label the picker offers is always one the
   * project already uses, and why creating one is just attaching a name.
   */
  readonly listLabels: (projectId: string) => Promise<NativeLabelList>;
  readonly transition: (input: {
    projectId: string;
    itemId: string;
    key: string;
    expectedRevision: string;
    state: string;
    reason?: TransitionReason;
  }) => Promise<NativeIssue>;
  readonly setDependency: (input: {
    projectId: string;
    itemId: string;
    key: string;
    expectedRevision: string;
    relatedWorkItemId: string;
    operation: "add" | "remove";
  }) => Promise<NativeIssue>;
  readonly listAttempts: (
    projectId: string,
    itemId: string,
    limit?: number,
  ) => Promise<AttemptPage>;
  /**
   * The latest stored diff on one issue (decisions.md §18.5), or `{diff: null}`
   * where no attempt has posted one. Issue-addressed rather than
   * attempt-addressed so that "this issue has no diff" is an answer rather
   * than a 404 per attempt.
   */
  readonly getWorkItemDiff: (projectId: string, itemId: string) => Promise<WorkItemDiff>;
  readonly listHistory: (input: {
    projectId: string;
    itemId: string;
    cursor?: string;
    limit?: number;
  }) => Promise<HistoryPage>;
  readonly listComments: (input: {
    projectId: string;
    itemId: string;
    cursor?: string;
    limit?: number;
  }) => Promise<CommentPage>;
  /**
   * Posts one work item comment. `POST .../work-items/:item/comments` carries
   * only the idempotency key and the body — a comment has no revision to be
   * composed against, so there is no `expected_revision` here and a concurrent
   * comment is not a conflict.
   */
  readonly createComment: (input: {
    projectId: string;
    itemId: string;
    key: string;
    body: string;
  }) => Promise<NativeComment>;
  readonly listChanges: (projectId: string, itemId: string) => Promise<ChangeRequestList>;
  readonly getChange: (
    projectId: string,
    itemId: string,
    changeId: string,
  ) => Promise<ChangeDetail>;
  /**
   * The issue's pull requests (decisions.md §18.6). A bare array like
   * `listChanges`, and served from a 60-second cache, so a caller that wants
   * the connector re-fetched has to ask for it — which this client does not.
   */
  readonly listPullRequests: (
    projectId: string,
    itemId: string,
  ) => Promise<WorkItemPullRequestList>;
  /**
   * Opens the issue's pull request (decisions.md §18.6).
   *
   * Addressed by the issue rather than by a number, because a pull request
   * that does not exist yet has none. `expectedHeadSha` is what the merge
   * queue checks before it acts: a head that moved since it was read comes
   * back 409 `head_moved` with both shas in `details`, and the caller re-reads
   * rather than forcing.
   */
  readonly openPullRequest: (input: {
    projectId: string;
    itemId: string;
    key: string;
    expectedHeadSha: string;
  }) => Promise<PullRequestActionAccepted>;

  // --- Workspace sessions (decisions.md §18.1, §18.2) -----------------------
  //
  // These live here rather than in their own client for one reason: minting a
  // relay ticket is a mutation, and `send` is the only place the `X-CSRF-Token`
  // the hosted boundary requires is attached. A second client would have to
  // copy that rule, and a copy of a security rule is a place for it to rot.

  readonly listWorkspaces: (input: {
    projectId: string;
    workItemId?: string;
    state?: string;
  }) => Promise<WorkspaceList>;
  readonly getWorkspace: (projectId: string, workspaceId: string) => Promise<Workspace>;
  readonly createWorkspace: (input: {
    projectId: string;
    key: string;
    workItemId?: string;
    attemptId?: string;
    ref?: string;
    requires?: readonly string[];
  }) => Promise<Workspace>;
  readonly closeWorkspace: (projectId: string, workspaceId: string) => Promise<void>;
  /**
   * One relay ticket. Single use and 30 seconds long (§18.2), so this is
   * called immediately before each socket is opened and never cached.
   */
  readonly mintRelayTicket: (input: {
    projectId: string;
    workspaceId: string;
    key: string;
  }) => Promise<RelayTicket>;
  /** The person-side relay socket, as an absolute same-origin `ws(s):` URL. */
  readonly relayUrl: (projectId: string, workspaceId: string, ticket: string) => string;

  // --- Project actions and their runs (decisions.md §18.12) -----------------
  //
  // Here for the same reason the workspace calls are: every one of the four
  // mutations is a mutation, and `send` is the only place the `X-CSRF-Token`
  // the hosted boundary requires is attached.

  readonly listActions: (projectId: string) => Promise<ActionList>;
  readonly createAction: (input: {
    projectId: string;
    key: string;
    name: string;
    command: string;
    keybinding?: string | null;
    icon?: string;
    previewUrl?: string | null;
    openPreview?: boolean;
    runOnWorktreeCreation?: boolean;
  }) => Promise<Action>;
  /**
   * `expected_revision` is not optional: §18.12 writes it into the contract,
   * and an edit that did not carry it would silently overwrite a colleague's.
   */
  readonly updateAction: (input: {
    projectId: string;
    actionId: string;
    key: string;
    expectedRevision: number | string;
    name?: string;
    command?: string;
    keybinding?: string | null;
    icon?: string;
    previewUrl?: string | null;
    openPreview?: boolean;
    runOnWorktreeCreation?: boolean;
  }) => Promise<Action>;
  readonly deleteAction: (projectId: string, actionId: string) => Promise<void>;
  /** 202, and the run row is already `queued` when it answers. */
  readonly startActionRun: (input: {
    projectId: string;
    actionId: string;
    key: string;
    workspaceId: string;
  }) => Promise<ActionRunAccepted>;
  readonly getActionRun: (
    projectId: string,
    actionId: string,
    runId: string,
  ) => Promise<ActionRun>;
  readonly listActionRuns: (projectId: string, actionId: string) => Promise<ActionRunList>;
  /** `text/plain`, not JSON: the run's own bytes, capped at 1 MiB (§18.12). */
  readonly readActionRunOutput: (
    projectId: string,
    actionId: string,
    runId: string,
  ) => Promise<string>;
}

const MUTATIONS = new Set(["POST", "PATCH", "PUT", "DELETE"]);

/**
 * The six action fields a create and a patch share.
 *
 * Absent means "leave alone" on a patch, so an undefined field is omitted
 * rather than sent as null. `keybinding` and `previewUrl` are the exception
 * and are sent when they are explicitly `null`: clearing a chord is a real
 * edit, and omitting it would be indistinguishable from not touching it.
 */
function actionFields(input: {
  keybinding?: string | null;
  icon?: string;
  previewUrl?: string | null;
  openPreview?: boolean;
  runOnWorktreeCreation?: boolean;
}): Record<string, unknown> {
  return {
    ...(input.keybinding === undefined ? {} : { keybinding: input.keybinding ?? "" }),
    ...(input.icon === undefined ? {} : { icon: input.icon }),
    ...(input.previewUrl === undefined ? {} : { preview_url: input.previewUrl ?? "" }),
    ...(input.openPreview === undefined ? {} : { open_preview: input.openPreview }),
    ...(input.runOnWorktreeCreation === undefined
      ? {}
      : { run_on_worktree_creation: input.runOnWorktreeCreation }),
  };
}

export function makeWorkHttp(options: WorkHttpOptions): WorkHttp {
  const doFetch: FetchLike = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
  const projectBase = (projectId: string) =>
    `${options.apiBase}/projects/${encodeURIComponent(projectId)}`;
  const itemBase = (projectId: string, itemId: string) =>
    `${projectBase(projectId)}/work-items/${encodeURIComponent(itemId)}`;
  const workspaceBase = (projectId: string, workspaceId: string) =>
    `${projectBase(projectId)}/workspaces/${encodeURIComponent(workspaceId)}`;
  const actionBase = (projectId: string, actionId: string) =>
    `${projectBase(projectId)}/actions/${encodeURIComponent(actionId)}`;

  const url = (path: string, query?: Record<string, string | number | boolean | undefined>) => {
    const search = new URLSearchParams();
    for (const [key, value] of Object.entries(query ?? {})) {
      if (value === undefined) continue;
      search.set(key, String(value));
    }
    const suffix = search.size > 0 ? `?${search.toString()}` : "";
    return `${options.origin}${path}${suffix}`;
  };

  async function send<A>(
    schema: Schema.Codec<A, any, never, never>,
    method: string,
    target: string,
    body?: unknown,
  ): Promise<A> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (MUTATIONS.has(method)) headers["X-CSRF-Token"] = options.csrfToken;
    let response: Response;
    try {
      response = await doFetch(target, {
        method,
        credentials: "same-origin",
        headers,
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
    } catch (cause) {
      throw new WorkApiError({
        status: 0,
        code: "network",
        message: cause instanceof Error ? cause.message : String(cause),
      });
    }
    const text = await response.text();
    let parsed: unknown = undefined;
    if (text.length > 0) {
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = undefined;
      }
    }
    if (!response.ok) throw errorFromBody(response.status, parsed);
    try {
      return Schema.decodeUnknownSync(schema)(parsed);
    } catch (cause) {
      throw new WorkApiError({
        status: response.status,
        code: "invalid",
        message: `The hub returned an unexpected payload: ${
          cause instanceof Error ? cause.message : String(cause)
        }`,
      });
    }
  }

  /**
   * The one read whose body is not JSON: a run's output is `text/plain`
   * (§18.12). The failure path is still `send`'s, because a refusal is a
   * native error body like every other.
   */
  async function sendText(target: string): Promise<string> {
    let response: Response;
    try {
      response = await doFetch(target, {
        method: "GET",
        credentials: "same-origin",
        headers: { Accept: "text/plain" },
      });
    } catch (cause) {
      throw new WorkApiError({
        status: 0,
        code: "network",
        message: cause instanceof Error ? cause.message : String(cause),
      });
    }
    const text = await response.text();
    if (response.ok) return text;
    let parsed: unknown = undefined;
    if (text.length > 0) {
      try {
        parsed = JSON.parse(text);
      } catch {
        parsed = undefined;
      }
    }
    throw errorFromBody(response.status, parsed);
  }

  return {
    origin: options.origin,
    apiBase: options.apiBase,
    eventsUrl: (projectId) =>
      `${options.origin}${hubPath(`/projects/${encodeURIComponent(projectId)}/events`)}`,
    getProject: (projectId) => send(NativeProject, "GET", url(projectBase(projectId))),
    listWorkItems: (input) =>
      send(
        WorkItemPage,
        "GET",
        url(`${projectBase(input.projectId)}/work-items`, {
          state: input.state,
          label: input.label,
          assignee: input.assignee,
          priority: input.priority,
          cursor: input.cursor,
          limit: input.limit,
          include: input.includeCoordinator === true ? "coordinator" : undefined,
        }),
      ),
    getWorkItem: (projectId, itemId) =>
      send(NativeIssue, "GET", url(itemBase(projectId, itemId))),
    listLabels: (projectId) =>
      send(NativeLabelList, "GET", url(`${projectBase(projectId)}/labels`)),
    patchWorkItem: (input) =>
      send(NativeIssue, "PATCH", url(itemBase(input.projectId, input.itemId)), {
        idempotency_key: input.key,
        expected_revision: input.expectedRevision,
        ...(input.title === undefined ? {} : { title: input.title }),
        ...(input.body === undefined ? {} : { body: input.body }),
        ...(input.priority === undefined ? {} : { priority: input.priority }),
        ...(input.labels === undefined ? {} : { labels: [...input.labels] }),
        ...(input.assignees === undefined ? {} : { assignees: [...input.assignees] }),
      }),
    transition: (input) =>
      send(NativeIssue, "POST", url(`${itemBase(input.projectId, input.itemId)}/workflow`), {
        idempotency_key: input.key,
        expected_revision: input.expectedRevision,
        state: input.state,
        // A person moving a card is `user_requested` and nothing else: the
        // other two reasons belong to the scheduler and claiming one here
        // would put a lie in the history.
        reason: input.reason ?? "user_requested",
      }),
    setDependency: (input) =>
      send(NativeIssue, "POST", url(`${itemBase(input.projectId, input.itemId)}/dependencies`), {
        idempotency_key: input.key,
        expected_revision: input.expectedRevision,
        related_work_item_id: input.relatedWorkItemId,
        operation: input.operation,
      }),
    listAttempts: (projectId, itemId, limit) =>
      send(AttemptPage, "GET", url(`${itemBase(projectId, itemId)}/attempts`, { limit })),
    getWorkItemDiff: (projectId, itemId) =>
      send(WorkItemDiff, "GET", url(`${itemBase(projectId, itemId)}/diff`)),
    listHistory: (input) =>
      send(HistoryPage, "GET", url(`${itemBase(input.projectId, input.itemId)}/history`, {
        cursor: input.cursor,
        limit: input.limit,
      })),
    listComments: (input) =>
      send(CommentPage, "GET", url(`${itemBase(input.projectId, input.itemId)}/comments`, {
        cursor: input.cursor,
        limit: input.limit,
      })),
    createComment: (input) =>
      send(NativeComment, "POST", url(`${itemBase(input.projectId, input.itemId)}/comments`), {
        idempotency_key: input.key,
        body: input.body,
      }),
    // The one native list with no envelope: a bare array, no cursor.
    listChanges: (projectId, itemId) =>
      send(ChangeRequestList, "GET", url(`${itemBase(projectId, itemId)}/changes`)),
    getChange: (projectId, itemId, changeId) =>
      send(
        ChangeDetail,
        "GET",
        url(`${itemBase(projectId, itemId)}/changes/${encodeURIComponent(changeId)}`),
      ),
    listPullRequests: (projectId, itemId) =>
      send(WorkItemPullRequestList, "GET", url(`${itemBase(projectId, itemId)}/pull-requests`)),
    openPullRequest: (input) =>
      send(
        PullRequestActionAccepted,
        "POST",
        url(`${itemBase(input.projectId, input.itemId)}/pull-requests/actions`),
        {
          idempotency_key: input.key,
          action: "open",
          expected_head_sha: input.expectedHeadSha,
        },
      ),

    listWorkspaces: (input) =>
      send(
        WorkspaceList,
        "GET",
        url(`${projectBase(input.projectId)}/workspaces`, {
          work_item: input.workItemId,
          state: input.state,
        }),
      ),
    getWorkspace: (projectId, workspaceId) =>
      send(Workspace, "GET", url(workspaceBase(projectId, workspaceId))),
    createWorkspace: (input) =>
      send(Workspace, "POST", url(`${projectBase(input.projectId)}/workspaces`), {
        idempotency_key: input.key,
        ...(input.workItemId === undefined ? {} : { work_item_id: input.workItemId }),
        ...(input.attemptId === undefined ? {} : { attempt_id: input.attemptId }),
        ...(input.ref === undefined ? {} : { ref: input.ref }),
        ...(input.requires === undefined ? {} : { requires: [...input.requires] }),
      }),
    // 204, no body: `Schema.Unknown` is what accepts the `undefined` that
    // parses out of an empty response without loosening any other decode.
    closeWorkspace: async (projectId, workspaceId) => {
      await send(Schema.Unknown, "DELETE", url(workspaceBase(projectId, workspaceId)));
    },
    mintRelayTicket: (input) =>
      send(
        RelayTicket,
        "POST",
        url(`${workspaceBase(input.projectId, input.workspaceId)}/relay-tickets`),
        { idempotency_key: input.key },
      ),
    listActions: (projectId) =>
      send(ActionList, "GET", url(`${projectBase(projectId)}/actions`)),
    createAction: (input) =>
      send(Action, "POST", url(`${projectBase(input.projectId)}/actions`), {
        idempotency_key: input.key,
        name: input.name,
        command: input.command,
        ...actionFields(input),
      }),
    updateAction: (input) =>
      send(Action, "PATCH", url(actionBase(input.projectId, input.actionId)), {
        idempotency_key: input.key,
        expected_revision: input.expectedRevision,
        ...(input.name === undefined ? {} : { name: input.name }),
        ...(input.command === undefined ? {} : { command: input.command }),
        ...actionFields(input),
      }),
    // 204, no body: `Schema.Unknown` accepts the `undefined` an empty response
    // parses to, exactly as `closeWorkspace` above does.
    deleteAction: async (projectId, actionId) => {
      await send(Schema.Unknown, "DELETE", url(actionBase(projectId, actionId)));
    },
    startActionRun: (input) =>
      send(
        ActionRunAccepted,
        "POST",
        url(`${actionBase(input.projectId, input.actionId)}/runs`),
        { idempotency_key: input.key, workspace_id: input.workspaceId },
      ),
    getActionRun: (projectId, actionId, runId) =>
      send(
        ActionRun,
        "GET",
        url(`${actionBase(projectId, actionId)}/runs/${encodeURIComponent(runId)}`),
      ),
    listActionRuns: (projectId, actionId) =>
      send(ActionRunList, "GET", url(`${actionBase(projectId, actionId)}/runs`)),
    readActionRunOutput: (projectId, actionId, runId) =>
      sendText(
        url(`${actionBase(projectId, actionId)}/runs/${encodeURIComponent(runId)}/output`),
      ),
    // A WebSocket needs an absolute URL with a `ws` scheme, and the ticket is
    // the CSRF answer for an upgrade a browser cannot put a header on (§18.2).
    relayUrl: (projectId, workspaceId, ticket) => {
      const path = `${workspaceBase(projectId, workspaceId)}/relay?ticket=${encodeURIComponent(
        ticket,
      )}`;
      const base =
        options.origin === ""
          ? (globalThis.location?.href ?? "http://localhost")
          : options.origin;
      const absolute = new URL(path, base);
      absolute.protocol = absolute.protocol === "https:" ? "wss:" : "ws:";
      return absolute.toString();
    },
  };
}

/** An idempotency key: one per intent, reused by every retry of that intent. */
export function newWorkKey(prefix = "work"): string {
  const random = globalThis.crypto?.randomUUID?.();
  return `${prefix}_${random ?? `${Date.now()}_${Math.random().toString(16).slice(2)}`}`.slice(
    0,
    128,
  );
}
