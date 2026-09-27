// Wire contracts for the hub's native work API.
//
// These are the shapes served under
// `/api/v2/organizations/:organization/projects/:project` by
// `internal/hubserver/native_pages.go`, `native_issues.go`,
// `native_attempts.go` and `changes.go`, and the types they marshal in
// `internal/tracker/native.go`. The fixtures under `fixtures/work-*.json` are
// the shared examples, read from both sides by the mechanism described in
// `fixtures.ts`.
//
// Three conventions of that API are easy to get wrong and are called out at
// every field that uses them:
//
//  - Go's `,string` tag puts an integer on the wire as a **JSON string**.
//    `revision`, `expected_revision`, `sequence`, `aggregate_sequence`,
//    `fencing_token` and a change version's `number` are all strings; `number`
//    on a work item, `priority` and `schema_version` are numbers.
//  - `omitempty` means a field is **absent**, never `null`. Those are
//    `Schema.optional`, not `Schema.NullOr`.
//  - a Go slice that was never initialised marshals as `null`. The handlers
//    initialise most of them, but `NativeState.transitions` can genuinely
//    arrive as `null`, so that one is nullable as well as optional.
import * as Schema from "effect/Schema";

// --- Identities -------------------------------------------------------------

export const OrganizationId = Schema.String;
export const ProjectId = Schema.String;
export const NativeWorkItemId = Schema.String;
export type NativeWorkItemId = typeof NativeWorkItemId.Type;
/** A revision is a positive integer that travels as a decimal string. */
export const Revision = Schema.String;
export type Revision = typeof Revision.Type;

// --- Shared members ---------------------------------------------------------

/** `human` for an operator's session, `runner` for a worker credential. */
export const NativeActorKind = Schema.Literals(["human", "runner"]);

export const NativeActor = Schema.Struct({
  kind: NativeActorKind,
  principal_id: Schema.String,
});
export type NativeActor = typeof NativeActor.Type;

/** Where a mirrored record came from. `provider` is `github` today. */
export const Provenance = Schema.Struct({
  provider: Schema.String,
  external_id: Schema.String,
  author_id: Schema.String,
  author_display_name: Schema.optional(Schema.String),
  created_at: Schema.optional(Schema.String),
  updated_at: Schema.optional(Schema.String),
  observed_at: Schema.optional(Schema.String),
});
export type Provenance = typeof Provenance.Type;

export const ExternalReference = Schema.Struct({
  provider: Schema.String,
  kind: Schema.String,
  id: Schema.String,
});
export type ExternalReference = typeof ExternalReference.Type;

/**
 * The pagination envelope every native list uses. `next_cursor` is absent —
 * not empty, not null — on the last page, and there is no total and no
 * `has_more`: "is there more" is exactly "did a cursor come back".
 */
export function Page<A, I>(item: Schema.Codec<A, I, never, never>) {
  return Schema.Struct({
    items: Schema.Array(item),
    next_cursor: Schema.optional(Schema.String),
  });
}

// --- Project and workflow ---------------------------------------------------

/**
 * One workflow state. The array order in `NativeProject.states` **is** the
 * lane order — there is no rank field — and `transitions` is the allow-list
 * the workflow endpoint enforces, so the board's move menu is built from it
 * rather than from every state.
 */
export const NativeState = Schema.Struct({
  /** Present only when true: the state refuses a worker credential. */
  operator_only: Schema.optional(Schema.Boolean),
  name: Schema.String,
  terminal: Schema.Boolean,
  dispatchable: Schema.Boolean,
  /** Null where a state was created without a transitions array. */
  transitions: Schema.optional(Schema.NullOr(Schema.Array(Schema.String))),
});
export type NativeState = typeof NativeState.Type;

export const NativeProject = Schema.Struct({
  project_id: ProjectId,
  organization_id: OrganizationId,
  name: Schema.String,
  /** `native` or `compatibility` (a GitHub-mirrored project). */
  profile: Schema.String,
  states: Schema.Array(NativeState),
  require_dependencies: Schema.Boolean,
});
export type NativeProject = typeof NativeProject.Type;

// --- Work items -------------------------------------------------------------

/** A blocker, hydrated with the state that decides whether it still blocks. */
export const NativeDependency = Schema.Struct({
  work_item_id: NativeWorkItemId,
  project_id: ProjectId,
  state: Schema.String,
  terminal: Schema.Boolean,
});
export type NativeDependency = typeof NativeDependency.Type;

export const NativeIssue = Schema.Struct({
  /** Present only when the project does not require dependencies. */
  ignore_dependencies: Schema.optional(Schema.Boolean),
  organization_id: OrganizationId,
  project_id: ProjectId,
  work_item_id: NativeWorkItemId,
  number: Schema.Number,
  revision: Revision,
  profile: Schema.String,
  title: Schema.String,
  body: Schema.String,
  state: Schema.String,
  terminal: Schema.Boolean,
  /** 0 Urgent, 1 High, 2 Normal, 3 Low. Absent when the issue has none. */
  priority: Schema.optional(Schema.Number),
  labels: Schema.Array(Schema.String),
  assignees: Schema.Array(Schema.String),
  actor: NativeActor,
  provenance: Schema.optional(Provenance),
  created_at: Schema.String,
  updated_at: Schema.String,
  dependencies: Schema.Array(NativeWorkItemId),
  /**
   * The same set as `dependencies` with state hydrated, minus any the caller's
   * grant does not cover. A non-terminal entry here is what makes the issue
   * Blocked; `dependencies` alone cannot answer that.
   */
  blockers: Schema.Array(NativeDependency),
  external_references: Schema.Array(ExternalReference),
});
export type NativeIssue = typeof NativeIssue.Type;

export const WorkItemPage = Page(NativeIssue);
export type WorkItemPage = typeof WorkItemPage.Type;

/** Priority as the hub numbers it, in the order the tracker ranks it. */
export const PRIORITY_NAMES = ["Urgent", "High", "Normal", "Low"] as const;

export function priorityName(priority: number | undefined): string | null {
  if (priority === undefined) return null;
  return PRIORITY_NAMES[priority] ?? null;
}

export function priorityValue(name: string): number | null {
  const index = PRIORITY_NAMES.findIndex(
    (candidate) => candidate.toLowerCase() === name.trim().toLowerCase(),
  );
  return index < 0 ? null : index;
}

// --- Labels -----------------------------------------------------------------

/**
 * The prefixes the hub owns.
 *
 * `priority:` and `effort:` are fields with rows of their own on the issue
 * page, and `detent:` names the reserved run-mode labels the hub refuses on a
 * write (`requireUnreservedLabels`). None of them is a label a reader may
 * attach, so none appears in the label picker or among an issue's pills. The
 * list matches `managedLabelPrefixes` in
 * `internal/hubserver/native_labels.go`, which is what the catalogue endpoint
 * filters with; this copy is what keeps a label the client already holds off
 * the row when the catalogue has not loaded.
 */
export const MANAGED_LABEL_PREFIXES = ["priority:", "effort:", "detent:"] as const;

export function isManagedLabel(label: string): boolean {
  const folded = label.trim().toLowerCase();
  if (folded.length === 0) return true;
  return MANAGED_LABEL_PREFIXES.some((prefix) => folded.startsWith(prefix));
}

/**
 * One entry of `GET {nativeBase}/labels`.
 *
 * The hub has no label table: the catalogue is the union of what the
 * project's work items carry, and `color` is derived from the name so the
 * same label is the same dot in every client without a column to store it in.
 * `count` is how many items carry it, which is what orders the suggestions.
 */
export const NativeLabel = Schema.Struct({
  name: Schema.String,
  color: Schema.String,
  count: Schema.Number,
});
export type NativeLabel = typeof NativeLabel.Type;

export const NativeLabelList = Schema.Struct({
  items: Schema.Array(NativeLabel),
});
export type NativeLabelList = typeof NativeLabelList.Type;

// --- Mutations --------------------------------------------------------------

/**
 * Every native mutation carries an idempotency key of at most 128 bytes. A
 * replay of the same key with a byte-identical body returns the stored
 * response; the same key with a different body is `409 idempotency_conflict`,
 * which is why a retry has to reuse the key and a *new* intent has to mint a
 * new one.
 */
export const MutationEnvelope = Schema.Struct({
  idempotency_key: Schema.String,
  lease_id: Schema.optional(Schema.String),
  fencing_token: Schema.optional(Schema.String),
});

export const CreateIssueRequest = Schema.Struct({
  idempotency_key: Schema.String,
  title: Schema.String,
  body: Schema.String,
  state: Schema.String,
  priority: Schema.optional(Schema.Number),
  labels: Schema.Array(Schema.String),
  assignees: Schema.Array(Schema.String),
});
export type CreateIssueRequest = typeof CreateIssueRequest.Type;

/**
 * A patch names the revision it was composed against. Anything else is a lost
 * update, and the hub says so with `409 revision_conflict` rather than taking
 * the last writer's word for it.
 *
 * An omitted member means "leave alone" — except `priority`, which is the one
 * field with three things to say: absent leaves it, a number sets it, and the
 * word `"none"` removes it (`tracker.PriorityPatch`). A JSON `null` clears it
 * too; the word is what this client sends, because it is the one a reader of
 * a request log can tell apart from an omission.
 */
export const PriorityPatch = Schema.Union([Schema.Number, Schema.Literal("none")]);
export type PriorityPatch = typeof PriorityPatch.Type;

export const UpdateIssueRequest = Schema.Struct({
  idempotency_key: Schema.String,
  expected_revision: Revision,
  title: Schema.optional(Schema.String),
  body: Schema.optional(Schema.String),
  priority: Schema.optional(PriorityPatch),
  labels: Schema.optional(Schema.Array(Schema.String)),
  assignees: Schema.optional(Schema.Array(Schema.String)),
});
export type UpdateIssueRequest = typeof UpdateIssueRequest.Type;

/**
 * The only reasons the hub accepts. A person dragging a card is
 * `user_requested`; the other two belong to the scheduler and a client must
 * never claim them.
 */
export const TransitionReason = Schema.Literals([
  "user_requested",
  "worker_progress",
  "dependency_ready",
]);
export type TransitionReason = typeof TransitionReason.Type;

export const TransitionRequest = Schema.Struct({
  idempotency_key: Schema.String,
  expected_revision: Revision,
  state: Schema.String,
  reason: TransitionReason,
});
export type TransitionRequest = typeof TransitionRequest.Type;

export const DependencyOperation = Schema.Literals(["add", "remove"]);

export const DependencyRequest = Schema.Struct({
  idempotency_key: Schema.String,
  expected_revision: Revision,
  related_work_item_id: NativeWorkItemId,
  operation: DependencyOperation,
});
export type DependencyRequest = typeof DependencyRequest.Type;

// --- Comments and history ---------------------------------------------------

export const NativeComment = Schema.Struct({
  comment_id: Schema.String,
  organization_id: OrganizationId,
  project_id: ProjectId,
  work_item_id: NativeWorkItemId,
  revision: Revision,
  sequence: Schema.String,
  body: Schema.String,
  actor: NativeActor,
  edited_by: Schema.optional(NativeActor),
  provenance: Schema.optional(Provenance),
  created_at: Schema.String,
  updated_at: Schema.String,
});
export type NativeComment = typeof NativeComment.Type;

export const CommentPage = Page(NativeComment);
export type CommentPage = typeof CommentPage.Type;

/** A run's identity: which role, on which backend, with which model. */
export const NativeExecutionIdentity = Schema.Struct({
  role: Schema.String,
  backend: Schema.String,
  model: Schema.String,
});
export type NativeExecutionIdentity = typeof NativeExecutionIdentity.Type;

export const NativeChangeReference = Schema.Struct({
  change_id: Schema.String,
  version_id: Schema.String,
  head_sha: Schema.String,
});
export type NativeChangeReference = typeof NativeChangeReference.Type;

export const NativeCheckpoint = Schema.Struct({
  resume: Schema.String,
  availability: Schema.String,
  storage: Schema.String,
  worktree_state: Schema.String,
  head_sha: Schema.optional(Schema.String),
  workspace_digest: Schema.optional(Schema.String),
  expected_head_sha: Schema.optional(Schema.String),
  external_effect: Schema.String,
  effect_state: Schema.String,
  effect_id: Schema.optional(Schema.String),
  change: Schema.optional(NativeChangeReference),
});
export type NativeCheckpoint = typeof NativeCheckpoint.Type;

export const NativeRunData = Schema.Struct({
  sequence: Schema.optional(Schema.String),
  identity: Schema.optional(NativeExecutionIdentity),
  machine_id: Schema.optional(Schema.String),
  runner_id: Schema.optional(Schema.String),
  session_id: Schema.optional(Schema.String),
  handoff: Schema.optional(NativeCheckpoint),
  lease_id: Schema.String,
  fencing_token: Schema.String,
  run_id: Schema.String,
  attempt_id: Schema.String,
  policy_id: Schema.String,
  outcome: Schema.optional(Schema.String),
  artifact_ids: Schema.optional(Schema.Array(Schema.String)),
});
export type NativeRunData = typeof NativeRunData.Type;

export const CollaborationData = Schema.Struct({
  change: Schema.optional(NativeChangeReference),
  run: Schema.optional(NativeRunData),
  revision: Schema.optional(Revision),
  fields: Schema.optional(Schema.Array(Schema.String)),
  comment_id: Schema.optional(Schema.String),
  related_work_item_id: Schema.optional(NativeWorkItemId),
  operation: Schema.optional(Schema.String),
  from_state: Schema.optional(Schema.String),
  to_state: Schema.optional(Schema.String),
  reason: Schema.optional(Schema.String),
});
export type CollaborationData = typeof CollaborationData.Type;

/**
 * `type` is deliberately an open string rather than a closed union. The hub's
 * enumeration is `issue.created`, `issue.edited`, `issue.cutover`,
 * `workflow.transitioned`, `dependency.changed`, `comment.created`,
 * `comment.edited`, `comment.imported`, `github.imported`, `change.created`,
 * `change.version_published`, `run.started`, `run.finished` and
 * `run.checkpointed` — but history is an append-only log the hub grows, and a
 * reader that rejects a whole page because one row is new would lose the
 * thirteen rows it does understand.
 */
export const CollaborationEvent = Schema.Struct({
  event_id: Schema.String,
  organization_id: OrganizationId,
  project_id: ProjectId,
  aggregate_type: Schema.String,
  aggregate_id: NativeWorkItemId,
  aggregate_sequence: Schema.String,
  type: Schema.String,
  schema_version: Schema.Number,
  recorded_at: Schema.String,
  actor: NativeActor,
  data: CollaborationData,
});
export type CollaborationEvent = typeof CollaborationEvent.Type;

export const HistoryPage = Page(CollaborationEvent);
export type HistoryPage = typeof HistoryPage.Type;

// --- Attempts ---------------------------------------------------------------

/**
 * `running` is derived at read time: the hub rewrites it to `interrupted` when
 * the backing lease has been released or has expired, so a card that says
 * "live" is saying the lease is live, not merely that a row exists.
 *
 * There is no elapsed time, no progress and no token usage on this resource.
 * Elapsed is `updated_at - started_at`; the other two are not served at all,
 * which is why the board draws no progress bar.
 */
export const AttemptStatus = Schema.Literals([
  "running",
  "succeeded",
  "failed",
  "cancelled",
  "interrupted",
]);
export type AttemptStatus = typeof AttemptStatus.Type;

export const NativeAttempt = Schema.Struct({
  sequence: Schema.optional(Schema.String),
  identity: Schema.optional(NativeExecutionIdentity),
  machine_id: Schema.optional(Schema.String),
  runner_id: Schema.optional(Schema.String),
  session_id: Schema.optional(Schema.String),
  handoff: Schema.optional(NativeCheckpoint),
  lease_id: Schema.String,
  fencing_token: Schema.String,
  run_id: Schema.String,
  attempt_id: Schema.String,
  policy_id: Schema.String,
  outcome: Schema.optional(Schema.String),
  artifact_ids: Schema.optional(Schema.Array(Schema.String)),
  status: AttemptStatus,
  started_at: Schema.String,
  updated_at: Schema.String,
  checkpoint: Schema.optional(NativeCheckpoint),
});
export type NativeAttempt = typeof NativeAttempt.Type;

export const AttemptPage = Page(NativeAttempt);
export type AttemptPage = typeof AttemptPage.Type;

// --- Stored attempt diffs (decisions.md §18.5) -------------------------------

/**
 * One changed file of a stored attempt diff.
 *
 * `patch` is the file's own `diff --git` section, which is what the runner
 * posts and what `@pierre/diffs`' `parsePatchFiles` reads. It is empty for
 * three distinguishable reasons the counts survive: `binary` content, a
 * `denied` path (the files denylist of §18.4, applied by path string to both
 * `path` and `old_path`), or a patch cut at the hub's 1 MB per-file bound and
 * reported `truncated`.
 */
export const AttemptDiffFile = Schema.Struct({
  path: Schema.String,
  old_path: Schema.optional(Schema.String),
  status: Schema.Literals(["added", "modified", "deleted", "renamed"]),
  additions: Schema.Number,
  deletions: Schema.Number,
  binary: Schema.Boolean,
  patch: Schema.String,
  truncated: Schema.Boolean,
  denied: Schema.Boolean,
});
export type AttemptDiffFile = typeof AttemptDiffFile.Type;

export const AttemptDiffProducer = Schema.Struct({
  kind: Schema.String,
  id: Schema.optional(Schema.String),
  runner_id: Schema.optional(Schema.String),
  lease_id: Schema.String,
  // A bare number on this endpoint. `NativeAttempt.fencing_token` is a string
  // because `tracker.NativeRunData` tags it `,string`; `tracker.DiffProducer`
  // does not, so the two differ on the wire and are modelled as they are.
  fencing_token: Schema.Number,
});

export const AttemptDiffGeneration = Schema.Struct({
  source: Schema.String,
  id: Schema.optional(Schema.String),
  seq: Schema.Number,
});

/**
 * One stored generation of one attempt's worktree, from
 * `GET {nativeBase}/attempts/:attempt/diff` (decisions.md §18.5).
 *
 * This is the diff the Diff surface draws. Unlike a change version — whose
 * code is one opaque artifact with no file names in it — an attempt diff
 * carries the file list, the counts and the patches, because the runner posts
 * it from the worktree before every `run.checkpointed` and before
 * `run.finished`.
 */
export const AttemptDiff = Schema.Struct({
  id: Schema.String,
  attempt_id: Schema.String,
  producer: AttemptDiffProducer,
  generation: AttemptDiffGeneration,
  base_sha: Schema.String,
  head_sha: Schema.String,
  files: Schema.Array(AttemptDiffFile),
  file_count: Schema.Number,
  patch_bytes: Schema.Number,
  truncated: Schema.Boolean,
  created_at: Schema.String,
});
export type AttemptDiff = typeof AttemptDiff.Type;

/**
 * `GET {nativeBase}/work-items/:item/diff`: the latest stored diff on one
 * issue, whichever attempt produced it, or an explicit absence.
 *
 * The attempt-addressed read answers `404` for an attempt that posted no diff,
 * which is right for a caller that named one attempt and wrong for a client
 * asking whether the issue has a diff at all — that client would walk the
 * attempt list and take a 404, and a browser console error, for every attempt
 * that never checkpointed. `diff: null` is the same fact without the noise.
 */
export const WorkItemDiff = Schema.Struct({
  diff: Schema.NullOr(AttemptDiff),
});
export type WorkItemDiff = typeof WorkItemDiff.Type;

// --- Changes ----------------------------------------------------------------

export const ChangeRequest = Schema.Struct({
  change_id: Schema.String,
  organization_id: OrganizationId,
  project_id: ProjectId,
  work_item_id: NativeWorkItemId,
  linked_issues: Schema.Array(NativeWorkItemId),
  title: Schema.String,
  body: Schema.String,
  current_version_id: Schema.String,
  revision: Revision,
  created_at: Schema.String,
  updated_at: Schema.String,
});
export type ChangeRequest = typeof ChangeRequest.Type;

/**
 * The changes list is the one native list with no envelope: a bare array, in
 * `rowid` order, with no cursor. Modelled honestly rather than wrapped, so a
 * reader of this file is not surprised by the shape at the call site.
 */
export const ChangeRequestList = Schema.Array(ChangeRequest);
export type ChangeRequestList = typeof ChangeRequestList.Type;

/**
 * The code artifact of a version. This is as close to a diff as the API gets:
 * an opaque URI and a digest. The hub serves no file list, no hunks and no
 * per-file counts, so the review dock reports what it has instead of drawing a
 * diff it cannot fetch.
 */
export const ChangeArtifact = Schema.Struct({
  kind: Schema.String,
  uri: Schema.String,
  sha256: Schema.String,
  availability: Schema.String,
});
export type ChangeArtifact = typeof ChangeArtifact.Type;

export const ChangeExternalReference = Schema.Struct({
  provider: Schema.String,
  id: Schema.String,
  url: Schema.String,
});
export type ChangeExternalReference = typeof ChangeExternalReference.Type;

export const ChangeCheckSpec = Schema.Struct({
  name: Schema.String,
  principal_id: Schema.String,
  workflow_id: Schema.String,
  workflow_sha256: Schema.String,
  source: Schema.String,
  max_age_seconds: Schema.Number,
});

export const ChangeCheckExpectation = Schema.Struct({
  name: Schema.String,
  principal_id: Schema.String,
  workflow_id: Schema.String,
  workflow_sha256: Schema.String,
  source: Schema.String,
  max_age_seconds: Schema.Number,
  check_run_id: Schema.String,
});

export const ChangeReviewPolicy = Schema.Struct({
  review_policy_id: Schema.String,
  policy_id: Schema.String,
  require_review: Schema.Boolean,
  required_checks: Schema.Array(ChangeCheckSpec),
});

/**
 * The policy descriptor is carried verbatim and read only for its gates, so it
 * is modelled loosely: it is the hub's own configuration object and every one
 * of its members is the hub's business, not this client's.
 */
export const PolicyDescriptor = Schema.Struct({
  schema: Schema.Number,
  policy_id: Schema.String,
  source_revision: Schema.String,
  source_digest: Schema.String,
  config_digest: Schema.String,
  profile: Schema.optional(Schema.String),
  requirements: Schema.Record(Schema.String, Schema.Unknown),
  gates: Schema.Record(Schema.String, Schema.Unknown),
});
export type PolicyDescriptor = typeof PolicyDescriptor.Type;

export const ChangeVersion = Schema.Struct({
  base_sha: Schema.String,
  head_sha: Schema.String,
  merge_base_sha: Schema.String,
  repository: Schema.String,
  code: ChangeArtifact,
  artifacts: Schema.Array(ChangeArtifact),
  run_id: Schema.optional(Schema.String),
  attempt_id: Schema.optional(Schema.String),
  policy_id: Schema.String,
  external: Schema.optional(ChangeExternalReference),
  version_id: Schema.String,
  change_id: Schema.String,
  /** A `,string` integer: `"1"` for the first published version. */
  number: Schema.String,
  policy: PolicyDescriptor,
  review_policy: ChangeReviewPolicy,
  checks: Schema.Array(ChangeCheckExpectation),
  actor: NativeActor,
  created_at: Schema.String,
});
export type ChangeVersion = typeof ChangeVersion.Type;

export const ChangeReview = Schema.Struct({
  review_id: Schema.String,
  version_id: Schema.String,
  decision: Schema.String,
  body: Schema.String,
  actor: NativeActor,
  created_at: Schema.String,
});
export type ChangeReview = typeof ChangeReview.Type;

export const ChangeCheck = Schema.Struct({
  check_run_id: Schema.String,
  head_sha: Schema.String,
  run_id: Schema.String,
  policy_id: Schema.String,
  config_digest: Schema.String,
  workflow_id: Schema.String,
  workflow_sha256: Schema.String,
  source: Schema.String,
  conclusion: Schema.String,
  completed_at: Schema.String,
  evidence: Schema.Array(ChangeArtifact),
  version_id: Schema.String,
  actor: NativeActor,
  received_at: Schema.String,
});
export type ChangeCheck = typeof ChangeCheck.Type;

export const ChangeDiscussion = Schema.Struct({
  comment_id: Schema.String,
  version_id: Schema.optional(Schema.String),
  body: Schema.String,
  actor: NativeActor,
  provenance: Schema.optional(Provenance),
  created_at: Schema.String,
});
export type ChangeDiscussion = typeof ChangeDiscussion.Type;

/**
 * The one field the PR chip reads. `status` is the rolled-up verdict the hub
 * computed from the native review, the mirrored external review and the
 * checks; `messages` says why when it is not `ready`.
 */
export const ChangeSummary = Schema.Struct({
  native_review: Schema.String,
  external_review: Schema.String,
  checks: Schema.String,
  status: Schema.String,
  messages: Schema.Array(Schema.String),
});
export type ChangeSummary = typeof ChangeSummary.Type;

/** The mirrored GitHub pull request, where one has been observed. */
export const PullRequestSummary = Schema.Record(Schema.String, Schema.Unknown);

export const ChangeDetail = Schema.Struct({
  external_snapshot: Schema.optional(PullRequestSummary),
  change: ChangeRequest,
  versions: Schema.Array(ChangeVersion),
  reviews: Schema.Array(ChangeReview),
  checks: Schema.Array(ChangeCheck),
  discussion: Schema.Array(ChangeDiscussion),
  summary: ChangeSummary,
});
export type ChangeDetail = typeof ChangeDetail.Type;

// --- Pull requests ----------------------------------------------------------

/**
 * One row of `GET {nativeBase}/work-items/:item/pull-requests` (decisions.md
 * §18.6): the hub's own change request joined with the GitHub connector's
 * projection of the pull request it was mirrored to.
 *
 * Only the fields this client reads are named. The connector half is what
 * carries `number`, `url` and `head.ref`, and it is absent for a project with
 * no GitHub connector — which is why `connector` is nullable and why a row can
 * arrive with `number: 0`: the change request exists, the pull request does
 * not yet.
 */
export const WorkItemPullRequestRef = Schema.Struct({
  ref: Schema.String,
  sha: Schema.optional(Schema.String),
  repository: Schema.optional(Schema.String),
});
export type WorkItemPullRequestRef = typeof WorkItemPullRequestRef.Type;

export const WorkItemPullRequestConnector = Schema.Struct({
  provider: Schema.String,
  repository: Schema.String,
  synchronized_at: Schema.optional(Schema.NullOr(Schema.String)),
});
export type WorkItemPullRequestConnector = typeof WorkItemPullRequestConnector.Type;

export const WorkItemPullRequest = Schema.Struct({
  id: Schema.String,
  change_id: Schema.String,
  number: Schema.Number,
  title: Schema.String,
  state: Schema.String,
  draft: Schema.Boolean,
  url: Schema.String,
  head: WorkItemPullRequestRef,
  base: WorkItemPullRequestRef,
  connector: Schema.optional(Schema.NullOr(WorkItemPullRequestConnector)),
  updated_at: Schema.String,
});
export type WorkItemPullRequest = typeof WorkItemPullRequest.Type;

/** A bare array, no envelope and no cursor, as the hub serves it. */
export const WorkItemPullRequestList = Schema.Array(WorkItemPullRequest);
export type WorkItemPullRequestList = typeof WorkItemPullRequestList.Type;

/**
 * `POST …/work-items/:id/pull-requests/actions` → 202 (§18.6).
 *
 * Opening a pull request has no number yet, so the action is addressed by the
 * issue. The answer names the action the merge queue will run and re-states
 * the work item; nothing here reads the work item, so it is `Schema.Unknown`
 * rather than a second decode of `NativeIssue` that a field change could
 * break for a caller that does not look at it.
 */
export const PullRequestActionAccepted = Schema.Struct({
  work_item: Schema.optional(Schema.Unknown),
  action_id: Schema.String,
});
export type PullRequestActionAccepted = typeof PullRequestActionAccepted.Type;

/** 409 on an action: the head moved since `expected_head_sha` was read. */
export const PULL_REQUEST_HEAD_MOVED = "head_moved";

// --- Workspace sessions (decisions.md §18.1) --------------------------------

/**
 * The eight states a workspace session moves through.
 *
 * A client never infers one: a transition is only ever learned from the
 * project event stream's `workspace.<state>` frame or from a read, because
 * §18.1 makes the subscription the contract — "a client observes readiness by
 * subscription and never by polling".
 */
export const WorkspaceState = Schema.Literals([
  "requested",
  "starting",
  "ready",
  "idle",
  "unreachable",
  "closing",
  "closed",
  "failed",
]);
export type WorkspaceState = typeof WorkspaceState.Type;

/** Why a workspace left the happy path. The surface shows it, unparaphrased. */
export const WorkspaceReason = Schema.Literals([
  "no_runner",
  "checkout_failed",
  "worktree_missing",
  "runner_restarted",
  "hub_restarted",
  "lease_lost",
  "capacity",
  "closed_by_actor",
  "expired",
]);
export type WorkspaceReason = typeof WorkspaceReason.Type;

/** What the runner that claimed this workspace actually reported it can do. */
export const WorkspaceCapabilities = Schema.Struct({
  terminal: Schema.Boolean,
  files: Schema.Boolean,
  diff: Schema.Boolean,
  preview: Schema.Boolean,
  /**
   * Running one project action non-interactively (§18.12). Its own capability
   * beside `terminal` rather than a use of it: a runner may serve one and not
   * the other, and the gate, the recording and the lifetime all differ.
   */
  exec: Schema.Boolean,
  /**
   * The `git` channel of §18.13. The hub emits it on every workspace
   * response, so it is required rather than optional; a runner that did not
   * report the capability sends `false`, and the header's git group is
   * disabled with that as the reason rather than sending frames that would
   * come back `forbidden`.
   */
  git: Schema.Boolean,
});
export type WorkspaceCapabilities = typeof WorkspaceCapabilities.Type;

/**
 * One workspace session (§18.1).
 *
 * `revision` is a plain number here rather than one of the `,string` integers
 * the rest of this API marshals (see the file header). Both are accepted, so a
 * hub that later adds the `,string` tag cannot break a deployed client over a
 * field no surface reads.
 */
export const Workspace = Schema.Struct({
  id: Schema.String,
  organization_id: Schema.String,
  project_id: Schema.String,
  work_item_id: Schema.String,
  attempt_id: Schema.optional(Schema.NullOr(Schema.String)),
  ref: Schema.String,
  head_sha: Schema.optional(Schema.NullOr(Schema.String)),
  runner_id: Schema.optional(Schema.NullOr(Schema.String)),
  machine_id: Schema.optional(Schema.NullOr(Schema.String)),
  /**
   * The runner machine's own hostname, and the absolute path of the worktree
   * on it (§18.13). Both are for the header's Open picker: it compares the
   * hostname against the page's host to decide whether handing the operating
   * system a `cursor://file/<path>` URL would reach a path that is actually
   * there, and `worktree_path` is the path it hands over. They are also what
   * `openInCwd` and `gitCwd` mean in the copied `chat/ChatHeader.tsx`, which
   * is why `ChatWorkspace` feeds them from here.
   *
   * Nullable and optional: a workspace that has not been claimed yet has
   * neither, and a runner that reports no hostname leaves the picker naming
   * "the runner's machine" instead.
   */
  machine_hostname: Schema.optional(Schema.NullOr(Schema.String)),
  worktree_path: Schema.optional(Schema.NullOr(Schema.String)),
  state: WorkspaceState,
  reason: Schema.optional(Schema.NullOr(WorkspaceReason)),
  requires: Schema.Array(Schema.String),
  capabilities: Schema.optional(Schema.NullOr(WorkspaceCapabilities)),
  isolation: Schema.optional(Schema.NullOr(Schema.Literals(["user", "container"]))),
  worktree: Schema.optional(Schema.NullOr(Schema.Literals(["retained", "fresh"]))),
  read_only: Schema.Boolean,
  idle_timeout_seconds: Schema.Number,
  expires_at: Schema.String,
  opened_at: Schema.optional(Schema.NullOr(Schema.String)),
  last_activity_at: Schema.optional(Schema.NullOr(Schema.String)),
  created_by: Schema.String,
  // Owners and admins only, and no surface reads it; declared so a decode does
  // not have to be lenient about the one field it would otherwise trip on.
  relay_sessions: Schema.optional(
    Schema.NullOr(Schema.Array(Schema.Record(Schema.String, Schema.Unknown))),
  ),
  revision: Schema.Union([Schema.Number, Schema.String]),
  created_at: Schema.String,
  updated_at: Schema.String,
});
export type Workspace = typeof Workspace.Type;

/** `GET …/workspaces` — an envelope with no cursor. */
export const WorkspaceList = Schema.Struct({
  workspaces: Schema.Array(Workspace),
});
export type WorkspaceList = typeof WorkspaceList.Type;

/**
 * `POST …/workspaces/:id/relay-tickets`. `expires_in` is seconds (30 in §18.2)
 * and the ticket is single use, bound to the workspace and to the session that
 * minted it — so every socket, including every reconnect, mints its own.
 */
export const RelayTicket = Schema.Struct({
  ticket: Schema.String,
  expires_in: Schema.Number,
});
export type RelayTicket = typeof RelayTicket.Type;

/**
 * The states a workspace cannot be reused from. `closing` is here with the two
 * genuinely terminal ones because §18.1 has no edge back out of it: a surface
 * that adopted a closing workspace would come up only to be told it closed.
 */
export const WORKSPACE_UNUSABLE_STATES: ReadonlySet<WorkspaceState> = new Set<WorkspaceState>([
  "closing",
  "closed",
  "failed",
]);

/** 409 on create: an open workspace already exists, named in `details`. */
export const WORKSPACE_EXISTS = "workspace_exists";
/** 422 on create: the organization's or the person's open-workspace cap. */
export const WORKSPACE_LIMIT = "workspace_limit";

// --- Project actions and their runs (decisions.md §18.12) -------------------

/**
 * One project action: a command the project wrote down.
 *
 * Stored per project rather than per issue or per workspace, which is what
 * makes it worth writing once — every conversation in the project offers the
 * same actions, and a worktree opened tomorrow runs the same setup as one
 * opened today.
 *
 * `keybinding` and `preview_url` are `omitempty` on the hub's own struct, so
 * both are declared optional here rather than as an empty string a client
 * would have to test for. `organization_id` and `project_id` are the same.
 */
export const Action = Schema.Struct({
  id: Schema.String,
  organization_id: Schema.optional(Schema.String),
  project_id: Schema.optional(Schema.String),
  name: Schema.String,
  command: Schema.String,
  /** A chord string (`mod+shift+t`). The hub stores it and never resolves it. */
  keybinding: Schema.optional(Schema.String),

  icon: Schema.String,
  /** Stored and echoed, never acted on: the Browser surface is deferred (§18.7). */
  preview_url: Schema.optional(Schema.String),
  open_preview: Schema.Boolean,
  run_on_worktree_creation: Schema.Boolean,
  created_by: Schema.String,
  revision: Schema.Union([Schema.Number, Schema.String]),
  created_at: Schema.String,
  updated_at: Schema.String,
});
export type Action = typeof Action.Type;

/** `GET …/actions` — the whole set in authoring order, capped at 50, no cursor. */
export const ActionList = Schema.Struct({
  items: Schema.Array(Action),
});
export type ActionList = typeof ActionList.Type;

/**
 * The four states a run reaches. `succeeded` is exit 0 and `failed` is
 * everything else, including a run whose outcome cannot be established —
 * §5's rule for a control with an unknown outcome, applied to a command.
 */
export const ActionRunStatus = Schema.Literals(["queued", "running", "succeeded", "failed"]);
export type ActionRunStatus = typeof ActionRunStatus.Type;

/**
 * One run of one action.
 *
 * `exit_code` is null until the process exits, so "exited 0" and "never
 * exited" are different facts rather than the same zero; a run that failed
 * without exiting says why in `reason` (`lease_lost`, `stream_closed`,
 * `killed`, `workspace_closed`).
 */
export const ActionRun = Schema.Struct({
  id: Schema.String,
  action_id: Schema.String,
  workspace_id: Schema.String,
  command: Schema.String,
  status: ActionRunStatus,
  exit_code: Schema.NullOr(Schema.Number),
  reason: Schema.optional(Schema.String),
  started_at: Schema.NullOr(Schema.String),
  finished_at: Schema.NullOr(Schema.String),
  /** `GET …/actions/:id/runs/:run/output`, present once there are bytes to read. */
  output_artifact: Schema.optional(Schema.String),
  output_bytes: Schema.Number,
  truncated: Schema.Boolean,
  created_by: Schema.optional(Schema.String),
  revision: Schema.Union([Schema.Number, Schema.String]),
  created_at: Schema.String,
  updated_at: Schema.String,
});
export type ActionRun = typeof ActionRun.Type;

/** `POST …/actions/:id/runs` → 202. The row is `queued` before any frame. */
export const ActionRunAccepted = Schema.Struct({
  run_id: Schema.String,
});
export type ActionRunAccepted = typeof ActionRunAccepted.Type;

/** `GET …/actions/:id/runs` — an action's runs, newest first. */
export const ActionRunList = Schema.Struct({
  items: Schema.Array(ActionRun),
});
export type ActionRunList = typeof ActionRunList.Type;

/**
 * The four `action_run.<status>` event names §18.12 puts on the project event
 * stream, with the run as `data` — the same stream `workspace.<state>` rides.
 * That is how a client observes a run it did not start (a
 * run-on-worktree-creation run, or one another tab began) by the same
 * subscription and never by polling.
 */
export const ACTION_RUN_EVENT_TYPES: readonly string[] = [
  "action_run.queued",
  "action_run.running",
  "action_run.succeeded",
  "action_run.failed",
];

/** 409 on a run: the workspace's runner does not serve the exec surface. */
export const ACTION_CAPABILITY_MISSING = "capability_missing";
/** 409 on a run: the attempt still holds this worktree, so it is read-only. */
export const ACTION_READ_ONLY = "read_only";

// --- Errors -----------------------------------------------------------------

/**
 * The native failure body.
 *
 * `current_revision` is the self-hosted shape only: in hosted mode
 * `nativeAPIError` rebuilds the body without it and replaces the message with
 * "The requested operation is unavailable". A hosted client therefore cannot
 * learn the current revision from a 409 and has to re-read the work item,
 * which is exactly what the board does after a conflict.
 */
export const NativeError = Schema.Struct({
  code: Schema.String,
  message: Schema.String,
  current_revision: Schema.optional(Revision),
  details: Schema.optional(Schema.Record(Schema.String, Schema.Unknown)),
});
export type NativeError = typeof NativeError.Type;

/** The codes the work surfaces act on rather than merely report. */
export const REVISION_CONFLICT = "revision_conflict";
export const IDEMPOTENCY_CONFLICT = "idempotency_conflict";
export const NOT_FOUND = "not_found";
