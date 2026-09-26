// Wire contracts for the Detent conversation product.
//
// These schemas are the client half of `docs/conversation/decisions.md` §5.
// The fixtures under `src/contracts/fixtures/` are the shared examples: the Go
// side is expected to marshal/unmarshal the same files, so a change here that
// is not mirrored there breaks a cross-language test on purpose.
import * as Schema from "effect/Schema";

// --- Identities -------------------------------------------------------------

export const ConversationId = Schema.String;
export type ConversationId = typeof ConversationId.Type;
export const MessageId = Schema.String;
export type MessageId = typeof MessageId.Type;
export const QuestionId = Schema.String;
export type QuestionId = typeof QuestionId.Type;
export const WorkItemId = Schema.String;
export type WorkItemId = typeof WorkItemId.Type;
/** Client-generated idempotency key, at most 128 bytes (decisions.md §2). */
export const COMMAND_KEY_MAX_BYTES = 128;

// --- Enumerations -----------------------------------------------------------

export const ConversationVisibility = Schema.Literals(["private", "shared"]);
export type ConversationVisibility = typeof ConversationVisibility.Type;

/**
 * Settled replaces Archive (decisions.md §13.9, §14). A conversation is
 * `active` or `settled`; the hub dropped `archived` with the rest of the
 * archive surface.
 */
export const ConversationStatus = Schema.Literals(["active", "settled"]);
export type ConversationStatus = typeof ConversationStatus.Type;

/** True for everything that is not `active`: the Settled shelf's membership. */
export function isSettledStatus(status: ConversationStatus): boolean {
  return status !== "active";
}

/**
 * Turn preferences (decisions.md §13.14, §14). `auto` means the project's
 * configured defaults resolved on the runner; an explicit value travels with
 * the turn and, for a linked issue, into its `detent-agent` block.
 */
export const TurnPreferences = Schema.Struct({
  model: Schema.String,
  reasoning_effort: Schema.String,
  access: Schema.String,
});
export type TurnPreferences = typeof TurnPreferences.Type;

/** The value every picker preselects, and the only one the hub may omit. */
export const AUTO_PREFERENCE = "auto";

export const DEFAULT_TURN_PREFERENCES: TurnPreferences = {
  model: AUTO_PREFERENCE,
  reasoning_effort: AUTO_PREFERENCE,
  access: AUTO_PREFERENCE,
};

/**
 * One choice a picker offers, as the bootstrap lists it (§14).
 *
 * A model choice carries four more fields, every one of them optional and
 * absent unless an enrolled runner's backend catalogue published it: the
 * reasoning ladder that model supports, the rung it defaults to, the provider
 * behind it, and whether the provider has named a successor. They are what
 * lets the effort picker show a model's own levels instead of one fixed list
 * for every model, and the model picker group by provider and shelve retired
 * models rather than mixing them in.
 */
export const PreferenceChoice = Schema.Struct({
  id: Schema.String,
  label: Schema.String,
  /** True for the option "auto" resolves to. Marks, never selects. */
  default: Schema.Boolean,
  /** The model's reasoning ladder, least to most. Absent for non-models. */
  efforts: Schema.optional(Schema.Array(Schema.String)),
  /** One of `efforts`, when the runner said which rung it would use. */
  default_effort: Schema.optional(Schema.String),
  provider: Schema.optional(Schema.String),
  legacy: Schema.optional(Schema.Boolean),
  /**
   * True for the model the reporting backend picks when it is given none.
   *
   * Not the same fact as `default`, which says what "auto" resolves to for
   * this hub: one is the provider's answer and the other is the operator's.
   * The picker leads its list with this one.
   */
  backend_default: Schema.optional(Schema.Boolean),
});
export type PreferenceChoice = typeof PreferenceChoice.Type;

export const PreferenceChoices = Schema.Struct({
  models: Schema.Array(PreferenceChoice),
  efforts: Schema.Array(PreferenceChoice),
  access: Schema.Array(PreferenceChoice),
});
export type PreferenceChoices = typeof PreferenceChoices.Type;

export const ExecutionStatus = Schema.Literals([
  "idle",
  "waiting_for_runner",
  "starting",
  "running",
  "waiting_input",
  "interrupting",
  "completed",
  "interrupted",
  "failed",
  "unknown",
]);
export type ExecutionStatus = typeof ExecutionStatus.Type;

export const MessageRole = Schema.Literals(["user", "assistant", "system"]);
export type MessageRole = typeof MessageRole.Type;

export const MessageKind = Schema.Literals([
  "text",
  "answer",
  "interrupt",
  "continue",
  "tool",
  "status",
]);
export type MessageKind = typeof MessageKind.Type;

/**
 * Where a bound runner's provider context came from (decisions.md §10.4).
 * `thread` resumed the provider thread, `transcript` was handed the last N
 * messages instead, and `""` is an execution that never bound a runner.
 */
export const ResumeSource = Schema.Literals(["thread", "transcript", ""]);
export type ResumeSource = typeof ResumeSource.Type;

/** Delivery ladder from decisions.md §5 ("Delivery ladder for a control"). */
export const DeliveryStatus = Schema.Literals([
  "saved",
  "queued",
  "sending",
  "sent",
  "delivered",
  "responding",
  "completed",
  "interrupted",
  "rejected",
  "failed",
  "unknown",
]);
export type DeliveryStatus = typeof DeliveryStatus.Type;

/** The receipt ladder is the prefix of the delivery ladder a command can reach. */
export const ReceiptStatus = Schema.Literals([
  "saved",
  "queued",
  "sending",
  "sent",
  "delivered",
  "rejected",
  "unknown",
]);
export type ReceiptStatus = typeof ReceiptStatus.Type;

export const QuestionStatus = Schema.Literals([
  "pending",
  "sending",
  "sent",
  "answered",
  "expired",
  "unknown",
]);
export type QuestionStatus = typeof QuestionStatus.Type;

export const ActorKind = Schema.Literals(["human", "runner", "coordinator"]);
export type ActorKind = typeof ActorKind.Type;

export const CommandKind = Schema.Literals([
  "message",
  "answer",
  "interrupt",
  "continue",
  "cancel",
  "retry",
]);
export type CommandKind = typeof CommandKind.Type;

export const ClosedReason = Schema.Literals([
  "access_revoked",
  "server_shutdown",
  "cursor_expired",
  /**
   * The hub could not keep this stream open. Unlike `access_revoked` it says
   * nothing about the conversation, so the client comes back — with backoff,
   * from the cursor it already holds.
   */
  "server_error",
]);
export type ClosedReason = typeof ClosedReason.Type;

// --- Errors -----------------------------------------------------------------

/**
 * Every code in the decisions.md §5 error table. `queue_full` is retryable;
 * `idempotency_conflict` and `stale_execution` are not (the payload or the
 * owner generation has to change first).
 */
export const ErrorCode = Schema.Literals([
  "not_found",
  "forbidden",
  "idempotency_conflict",
  "stale_execution",
  "conversation_already_linked",
  "question_already_answered",
  "invalid_request",
  "unsupported_control",
  "share_history_required",
  "conversation_project_fixed",
  "queue_full",
]);
export type ErrorCode = typeof ErrorCode.Type;

export const ApiError = Schema.Struct({
  code: Schema.String,
  message: Schema.String,
  details: Schema.optional(Schema.Unknown),
});
export type ApiError = typeof ApiError.Type;

export const RETRYABLE_ERROR_CODES: ReadonlySet<string> = new Set(["queue_full"]);

export function isRetryableErrorCode(code: string): boolean {
  return RETRYABLE_ERROR_CODES.has(code);
}

// --- Resources --------------------------------------------------------------

export const Owner = Schema.Struct({
  principal_id: Schema.String,
  subject: Schema.String,
});
export type Owner = typeof Owner.Type;

/** `expected` on a command envelope: the owner generation the client saw. */
export const Expected = Schema.Struct({
  attempt_id: Schema.NullOr(Schema.String),
  turn_id: Schema.NullOr(Schema.String),
});
export type Expected = typeof Expected.Type;

export const Capabilities = Schema.Struct({
  steer: Schema.Boolean,
  interrupt: Schema.Boolean,
  answer: Schema.Boolean,
  continue: Schema.Boolean,
});
export type Capabilities = typeof Capabilities.Type;

export const Execution = Schema.Struct({
  status: ExecutionStatus,
  attempt_id: Schema.NullOr(Schema.String),
  run_id: Schema.NullOr(Schema.String),
  runner_id: Schema.NullOr(Schema.String),
  thread_id: Schema.NullOr(Schema.String),
  turn_id: Schema.NullOr(Schema.String),
  capabilities: Capabilities,
  /**
   * Transcript recovery is visible (decisions.md §10.4): a runner that could
   * not resume the provider thread continued from a transcript, and the strip
   * says so rather than letting the reader assume the earlier context was
   * there.
   */
  resume: ResumeSource,
  error: Schema.NullOr(Schema.String),
  updated_at: Schema.String,
});
export type Execution = typeof Execution.Type;

/**
 * The linked issue as the conversation carries it: enough to render and open
 * the issue card without a second request. `runner_bound` says an attempt
 * currently owns the issue, which is what the client shows instead of
 * "waiting for runner" (decisions.md §3 rule 6).
 */
export const WorkItem = Schema.Struct({
  id: WorkItemId,
  identifier: Schema.String,
  title: Schema.String,
  lane: Schema.String,
  runner_bound: Schema.Boolean,
});
export type WorkItem = typeof WorkItem.Type;

export const Conversation = Schema.Struct({
  id: ConversationId,
  organization_id: Schema.String,
  project_id: Schema.String,
  title: Schema.String,
  visibility: ConversationVisibility,
  status: ConversationStatus,
  work_item_id: Schema.NullOr(WorkItemId),
  linked_at: Schema.NullOr(Schema.String),
  work_item: Schema.NullOr(WorkItem),
  /** The turn preferences the composer's pickers show (§13.14). */
  preferences: TurnPreferences,
  owner: Owner,
  execution: Execution,
  revision: Schema.Number,
  event_seq: Schema.Number,
  /**
   * Every message in the conversation, not only the loaded page. The handoff
   * form states this number: linking makes the whole history readable, so an
   * audience preview that counted the loaded window would understate what is
   * being shared (decisions.md §10.5).
   */
  message_count: Schema.Number,
  created_at: Schema.String,
  updated_at: Schema.String,
  last_message_at: Schema.NullOr(Schema.String),
});
export type Conversation = typeof Conversation.Type;

/**
 * One resolved cross-reference (§14). `label` is the exact run of text in the
 * message — `#123` or `org/project#123` — and `url` is where it points.
 */
export const ReferenceKind = Schema.Literals(["issue", "conversation"]);
export type ReferenceKind = typeof ReferenceKind.Type;

export const MessageReference = Schema.Struct({
  kind: ReferenceKind,
  id: Schema.String,
  label: Schema.String,
  url: Schema.String,
});
export type MessageReference = typeof MessageReference.Type;

export const MessageActor = Schema.Struct({
  kind: ActorKind,
  principal_id: Schema.String,
});
export type MessageActor = typeof MessageActor.Type;

/**
 * A file staged on a conversation and then sent with a message
 * (decisions.md §17.1). The hub stores the bytes through the artifact service
 * and serves `url` under the conversation's own audience rule, so a link is
 * only as reachable as the conversation it belongs to.
 */
export const MessageAttachment = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  mime: Schema.String,
  size: Schema.Number,
  url: Schema.String,
});
export type MessageAttachment = typeof MessageAttachment.Type;

/** `POST .../conversations/:id/attachments` → 201. */
export const AttachmentUpload = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  mime: Schema.String,
  size: Schema.Number,
  url: Schema.String,
  expires_at: Schema.String,
});
export type AttachmentUpload = typeof AttachmentUpload.Type;

/** What the composer accepts (decisions.md §17.1). */
export const ATTACHMENT_MAX_BYTES = 20 * 1024 * 1024;
export const ATTACHMENT_MAX_PER_MESSAGE = 10;

/**
 * Images and text. The runner receives exactly these as provider input, so a
 * file the provider cannot read is refused at the composer rather than
 * uploaded and silently dropped on the way to a turn.
 */
export function attachmentTypeAccepted(mime: string, name: string): boolean {
  const type = mime.trim().toLowerCase();
  if (type.startsWith("image/")) return true;
  if (type.startsWith("text/")) return true;
  if (type === "application/json" || type === "application/xml") return true;
  if (type === "" || type === "application/octet-stream") {
    // Some browsers report no type for a known-good extension; fall back to it
    // rather than refusing a file the reader can plainly see is text.
    return /\.(md|markdown|txt|json|ya?ml|toml|csv|tsv|log|diff|patch)$/i.test(name);
  }
  return false;
}

export const Message = Schema.Struct({
  id: MessageId,
  conversation_id: ConversationId,
  seq: Schema.Number,
  role: MessageRole,
  kind: MessageKind,
  text: Schema.String,
  data: Schema.Record(Schema.String, Schema.Unknown),
  delivery: DeliveryStatus,
  attempt_id: Schema.NullOr(Schema.String),
  turn_id: Schema.NullOr(Schema.String),
  provider_item_id: Schema.NullOr(Schema.String),
  actor: MessageActor,
  command_key: Schema.NullOr(Schema.String),
  /**
   * Cross-references the hub extracted from the text on accept (§13.7, §14).
   * Always an array; the client renders each one as a link at the place its
   * `label` appears and leaves the text alone where there are none.
   */
  references: Schema.Array(MessageReference),
  /**
   * The files sent with this message (§17.1). Optional on the wire so a
   * message written before attachments existed still decodes.
   */
  attachments: Schema.optional(Schema.Array(MessageAttachment)),
  created_at: Schema.String,
  updated_at: Schema.String,
});
export type Message = typeof Message.Type;

export const QuestionOption = Schema.Struct({
  label: Schema.String,
  description: Schema.String,
});
export type QuestionOption = typeof QuestionOption.Type;

/**
 * One prompt inside a question group.
 *
 * `multiple` is optional and absent means a single choice: the answers map is
 * an array per prompt, so the wire shape already carries more than one value,
 * but only a prompt that says so renders checkboxes instead of radios.
 */
export const Prompt = Schema.Struct({
  id: Schema.String,
  header: Schema.String,
  question: Schema.String,
  options: Schema.Array(QuestionOption),
  free_text: Schema.Boolean,
  multiple: Schema.optional(Schema.Boolean),
});
export type Prompt = typeof Prompt.Type;

export const QuestionOwner = Schema.Struct({
  attempt_id: Schema.NullOr(Schema.String),
  turn_id: Schema.NullOr(Schema.String),
});
export type QuestionOwner = typeof QuestionOwner.Type;

export const Answers = Schema.Record(Schema.String, Schema.Array(Schema.String));
export type Answers = typeof Answers.Type;

export const Question = Schema.Struct({
  id: QuestionId,
  conversation_id: ConversationId,
  message_id: MessageId,
  status: QuestionStatus,
  owner: QuestionOwner,
  questions: Schema.Array(Prompt),
  answers: Answers,
  answered_by: Schema.NullOr(Schema.String),
  expires_at: Schema.NullOr(Schema.String),
  created_at: Schema.String,
  updated_at: Schema.String,
});
export type Question = typeof Question.Type;

export const Receipt = Schema.Struct({
  key: Schema.String,
  kind: Schema.String,
  status: ReceiptStatus,
  message_id: Schema.NullOr(MessageId),
  question_id: Schema.NullOr(QuestionId),
  error: Schema.NullOr(ApiError),
  updated_at: Schema.String,
});
export type Receipt = typeof Receipt.Type;

// --- Command envelope -------------------------------------------------------

const commandBase = {
  key: Schema.String,
  expected: Schema.optional(Expected),
};

export const MessageCommand = Schema.Struct({
  ...commandBase,
  kind: Schema.Literal("message"),
  text: Schema.String,
  /** Attachment ids, already uploaded and still unsent (§17.1). */
  attachments: Schema.optional(Schema.Array(Schema.String)),
});
export type MessageCommand = typeof MessageCommand.Type;

export const AnswerCommand = Schema.Struct({
  ...commandBase,
  kind: Schema.Literal("answer"),
  question_id: QuestionId,
  answers: Answers,
});
export type AnswerCommand = typeof AnswerCommand.Type;

export const InterruptCommand = Schema.Struct({
  ...commandBase,
  kind: Schema.Literal("interrupt"),
});
export type InterruptCommand = typeof InterruptCommand.Type;

export const ContinueCommand = Schema.Struct({
  ...commandBase,
  kind: Schema.Literal("continue"),
  text: Schema.optional(Schema.String),
});
export type ContinueCommand = typeof ContinueCommand.Type;

export const CancelCommand = Schema.Struct({
  ...commandBase,
  kind: Schema.Literal("cancel"),
});
export type CancelCommand = typeof CancelCommand.Type;

/**
 * Re-queue one message whose delivery could not be established
 * (decisions.md §10.3). It carries a *new* key every time and names the
 * message it re-queues: the hub sets that same message back to `queued` (or
 * `saved`), so nothing is duplicated. It is refused with `invalid_request`
 * unless the message's delivery is `unknown`, `failed` or `rejected`.
 */
export const RetryCommand = Schema.Struct({
  ...commandBase,
  kind: Schema.Literal("retry"),
  message_id: MessageId,
});
export type RetryCommand = typeof RetryCommand.Type;

export const Command = Schema.Union([
  MessageCommand,
  AnswerCommand,
  InterruptCommand,
  ContinueCommand,
  CancelCommand,
  RetryCommand,
]);
export type Command = typeof Command.Type;

// --- Events -----------------------------------------------------------------

export const MessageDelta = Schema.Struct({
  message_id: MessageId,
  seq: Schema.Number,
  text: Schema.String,
});
export type MessageDelta = typeof MessageDelta.Type;

export const Heartbeat = Schema.Struct({ seq: Schema.Number });
export type Heartbeat = typeof Heartbeat.Type;

export const Closed = Schema.Struct({ reason: ClosedReason });
export type Closed = typeof Closed.Type;

export const ConversationEvent = Schema.Union([
  Schema.Struct({ type: Schema.Literal("conversation.updated"), data: Conversation }),
  Schema.Struct({ type: Schema.Literal("message.accepted"), data: Message }),
  Schema.Struct({ type: Schema.Literal("message.delta"), data: MessageDelta }),
  Schema.Struct({ type: Schema.Literal("message.updated"), data: Message }),
  Schema.Struct({ type: Schema.Literal("question.opened"), data: Question }),
  Schema.Struct({ type: Schema.Literal("question.updated"), data: Question }),
  Schema.Struct({ type: Schema.Literal("execution.updated"), data: Execution }),
  Schema.Struct({ type: Schema.Literal("command.receipt"), data: Receipt }),
  Schema.Struct({ type: Schema.Literal("heartbeat"), data: Heartbeat }),
  Schema.Struct({ type: Schema.Literal("closed"), data: Closed }),
]);
export type ConversationEvent = typeof ConversationEvent.Type;

export const EVENT_TYPES = [
  "conversation.updated",
  "message.accepted",
  "message.delta",
  "message.updated",
  "question.opened",
  "question.updated",
  "execution.updated",
  "command.receipt",
  "heartbeat",
  "closed",
] as const;
export type EventType = (typeof EVENT_TYPES)[number];

/** An SSE frame carrying its `id:` cursor. Heartbeats have no id (§5). */
export interface ConversationFrame {
  readonly seq: number | null;
  readonly event: ConversationEvent;
}

// --- Responses --------------------------------------------------------------

export const ConversationSnapshot = Schema.Struct({
  conversation: Conversation,
  messages: Schema.Array(Message),
  questions: Schema.Array(Question),
  cursor: Schema.Number,
  has_more: Schema.optional(Schema.Boolean),
});
export type ConversationSnapshot = typeof ConversationSnapshot.Type;

export const ConversationListResponse = Schema.Struct({
  conversations: Schema.Array(Conversation),
  next_cursor: Schema.NullOr(Schema.String),
});
export type ConversationListResponse = typeof ConversationListResponse.Type;

export const MessagePageResponse = Schema.Struct({
  messages: Schema.Array(Message),
  next_cursor: Schema.NullOr(Schema.String),
});
export type MessagePageResponse = typeof MessagePageResponse.Type;

/**
 * The first message of a new conversation. Its `key` is the message command
 * key and stays distinct from the create key (decisions.md §10.2).
 */
export const FirstMessageInput = Schema.Struct({
  key: Schema.String,
  text: Schema.String,
});
export type FirstMessageInput = typeof FirstMessageInput.Type;

/**
 * `POST /conversations`. The top-level `key` is mandatory: creating a
 * conversation runs through the native idempotent mutation path keyed by actor
 * and key, so a retried create returns the stored response instead of a second
 * conversation, and the same key with a different payload is
 * `idempotency_conflict` (decisions.md §10.2).
 */
export const CreateConversationRequest = Schema.Struct({
  key: Schema.String,
  title: Schema.optional(Schema.String),
  first_message: Schema.optional(FirstMessageInput),
});
export type CreateConversationRequest = typeof CreateConversationRequest.Type;

export const CreateConversationResponse = Schema.Struct({
  conversation: Conversation,
  receipt: Schema.optional(Receipt),
});
export type CreateConversationResponse = typeof CreateConversationResponse.Type;

export const LinkedIssue = Schema.Struct({
  id: WorkItemId,
  identifier: Schema.String,
  title: Schema.String,
  state: Schema.String,
});
export type LinkedIssue = typeof LinkedIssue.Type;

/** The issue half of a link request body (decisions.md §5, `POST .../link`). */
export const LinkIssueInput = Schema.Struct({
  title: Schema.String,
  description: Schema.String,
  state: Schema.optional(Schema.String),
  labels: Schema.optional(Schema.Array(Schema.String)),
  /** The tracker's own rank, 0-3 (decisions.md §14). */
  priority: Schema.optional(Schema.Number),
});
export type LinkIssueInput = typeof LinkIssueInput.Type;

/**
 * The whole link request. `share_history` is mandatory and must be `true`:
 * linking makes the entire history readable by every project reader
 * (decisions.md §3 rule 2), and the hub rejects anything else with
 * `share_history_required`.
 */
export const HandoffDispatch = Schema.Literals(["now", "later"]);
export type HandoffDispatch = typeof HandoffDispatch.Type;

/**
 * The next step as the request carries it. Every field is optional: the hub
 * fills an unset one from the project's own defaults, so a form that has
 * nothing to say about a field says nothing rather than guessing.
 * `priority` is the native rank, 0-3.
 */
export const HandoffNext = Schema.Struct({
  state: Schema.optional(Schema.String),
  priority: Schema.optional(Schema.Number),
  dispatch: Schema.optional(HandoffDispatch),
});
export type HandoffNext = typeof HandoffNext.Type;

/** The next step the hub actually applied, echoed on the link response. */
export const HandoffNextApplied = Schema.Struct({
  state: Schema.String,
  priority: Schema.NullOr(Schema.Number),
  dispatch: HandoffDispatch,
});
export type HandoffNextApplied = typeof HandoffNextApplied.Type;

export const LinkRequest = Schema.Struct({
  key: Schema.String,
  share_history: Schema.Boolean,
  issue: LinkIssueInput,
  /**
   * What happens to the issue once it exists (§13.8, §14). The form
   * preselects the project's defaults, so this is always sent.
   */
  next: Schema.optional(HandoffNext),
});
export type LinkRequest = typeof LinkRequest.Type;

export const LinkResponse = Schema.Struct({
  conversation: Conversation,
  issue: LinkedIssue,
  scheduling: Schema.Struct({
    lane: Schema.String,
    runner_bound: Schema.Boolean,
  }),
  /** What the hub did with the next step it was given (§14). */
  next: HandoffNextApplied,
});
export type LinkResponse = typeof LinkResponse.Type;

// --- Structured message payloads -------------------------------------------
//
// A `kind: status` message carries its structure in `data`. The three shapes
// below are the ones the client renders as something other than a status row.
// Each is read leniently: an unknown or malformed payload renders as the
// message's own text rather than failing the transcript.

/** `data.proposal`: the coordinator suggesting an issue for this chat (U06). */
export const IssueProposal = Schema.Struct({
  project_id: Schema.String,
  title: Schema.String,
  objective: Schema.String,
});
export type IssueProposal = typeof IssueProposal.Type;

/** `data.issue`: the issue a completed handoff produced (U06 result card). */
export const IssueResult = Schema.Struct({
  id: WorkItemId,
  identifier: Schema.String,
  title: Schema.String,
  state: Schema.String,
  lane: Schema.optional(Schema.String),
  runner_bound: Schema.optional(Schema.Boolean),
});
export type IssueResult = typeof IssueResult.Type;

/** One entry of `data.attention`: an issue that wants the reader (U08). */
export const AttentionItem = Schema.Struct({
  work_item_id: WorkItemId,
  title: Schema.String,
  state: Schema.String,
  reason: Schema.String,
  url: Schema.optional(Schema.String),
});
export type AttentionItem = typeof AttentionItem.Type;

export const AttentionList = Schema.Array(AttentionItem);

/** `details` of a `conversation_already_linked` failure (decisions.md §3.4). */
export const AlreadyLinkedDetails = Schema.Struct({
  existing_conversation_id: ConversationId,
});
export type AlreadyLinkedDetails = typeof AlreadyLinkedDetails.Type;

function lenient<A>(schema: Schema.Codec<A, any, never, never>): (value: unknown) => A | undefined {
  const decode = Schema.decodeUnknownSync(schema);
  return (value) => {
    if (value === undefined || value === null) return undefined;
    try {
      return decode(value);
    } catch {
      return undefined;
    }
  };
}

const proposalOf = lenient(IssueProposal);
const issueOf = lenient(IssueResult);
const attentionOf = lenient(AttentionList);
const alreadyLinkedOf = lenient(AlreadyLinkedDetails);

export function readIssueProposal(data: Record<string, unknown>): IssueProposal | undefined {
  return proposalOf(data.proposal);
}

export function readIssueResult(data: Record<string, unknown>): IssueResult | undefined {
  return issueOf(data.issue);
}

export function readAttention(data: Record<string, unknown>): readonly AttentionItem[] {
  return attentionOf(data.attention) ?? [];
}

export function readAlreadyLinked(details: unknown): AlreadyLinkedDetails | undefined {
  return alreadyLinkedOf(details);
}

/**
 * One "referenced from" entry on an issue
 * (`GET {nativeBase}/work-items/:item/references`, §14).
 */
export const WorkItemReference = Schema.Struct({
  message_id: MessageId,
  conversation_id: ConversationId,
  excerpt: Schema.String,
  created_at: Schema.String,
});
export type WorkItemReference = typeof WorkItemReference.Type;

export const WorkItemReferencesResponse = Schema.Struct({
  references: Schema.Array(WorkItemReference),
});
export type WorkItemReferencesResponse = typeof WorkItemReferencesResponse.Type;

// --- Bootstrap --------------------------------------------------------------

/**
 * A project the actor can reach. `labels` and `priorities` are optional: the
 * handoff form offers those fields only when the bootstrap says the project
 * has them, and never invents a taxonomy of its own.
 *
 * `coordinator` is the per-project answer to the same question the top-level
 * `capabilities.coordinator` answers for the organization: is a runner able to
 * take coordinator turns enrolled here, or does a hub backend exist
 * (decisions.md §1, §9.2). It is optional because the hub does not publish it
 * yet; where it is absent the organization-wide capability stands.
 */
/** A workflow state, as the hub marshals `tracker.NativeState`. */
export const BootstrapState = Schema.Struct({
  name: Schema.String,
  terminal: Schema.optional(Schema.Boolean),
  dispatchable: Schema.optional(Schema.Boolean),
});
export type BootstrapState = typeof BootstrapState.Type;

export const BootstrapProject = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  can_write: Schema.Boolean,
  /**
   * The project grant's `runners` flag (§18.3). The hub has always sent it;
   * it is optional here for the reason `states` is, so one payload decodes
   * through this schema and the account shell's. Absent reads as absent
   * rather than as false, and the Terminal card says the grant is missing —
   * which is the honest answer for a client that was not told.
   */
  can_manage_runners: Schema.optional(Schema.Boolean),
  /** The project's workflow, for the handoff form's next-step state. */
  states: Schema.optional(Schema.Array(BootstrapState)),
  labels: Schema.optional(Schema.Array(Schema.String)),
  priorities: Schema.optional(Schema.Array(Schema.String)),
  coordinator: Schema.optional(Schema.Boolean),
});
export type BootstrapProject = typeof BootstrapProject.Type;

export const BootstrapActor = Schema.Struct({
  principal_id: Schema.String,
  subject: Schema.String,
  email: Schema.String,
  role: Schema.String,
});
export type BootstrapActor = typeof BootstrapActor.Type;

export const Bootstrap = Schema.Struct({
  organization: Schema.Struct({ id: Schema.String, name: Schema.String }),
  actor: BootstrapActor,
  projects: Schema.Array(BootstrapProject),
  csrf_token: Schema.String,
  capabilities: Schema.Struct({
    coordinator: Schema.Boolean,
    attachments: Schema.Boolean,
  }),
  api_base: Schema.String,
  /**
   * The choices the composer's pickers offer (§14). The hub always sends it;
   * it is optional here because the shared contract fixture cannot carry it
   * yet — the Go half's fixture value leaves `appBootstrapPreferences` unset,
   * and the cross-language test compares what both sides actually produce.
   * Absent means the pickers offer "Auto" alone, which is what a hub with no
   * enrolled runner would publish anyway.
   */
  preferences: Schema.optional(PreferenceChoices),
  /** The hub only serves this client when the conversation feature is on. */
  feature: Schema.Struct({ conversation: Schema.Boolean }),
});
export type Bootstrap = typeof Bootstrap.Type;

/**
 * The options one picker shows: "Auto" first and always, then whatever the
 * bootstrap published, with anything already chosen kept even if the hub has
 * since stopped offering it — a conversation must never silently change the
 * model it is running under because a runner went away.
 */
export function preferenceOptions(
  published: readonly PreferenceChoice[] | undefined,
  selected: string,
): readonly PreferenceChoice[] {
  const options: PreferenceChoice[] = [];
  if (!(published ?? []).some((choice) => choice.id === AUTO_PREFERENCE)) {
    options.push({ id: AUTO_PREFERENCE, label: "Auto", default: false });
  }
  options.push(...(published ?? []));
  if (selected !== AUTO_PREFERENCE && !options.some((option) => option.id === selected)) {
    options.push({ id: selected, label: selected, default: false });
  }
  return options;
}

/** The label a picker's trigger shows for the current value. */
export function preferenceLabel(
  published: readonly PreferenceChoice[] | undefined,
  selected: string,
): string {
  return (
    preferenceOptions(published, selected).find((option) => option.id === selected)?.label ?? "Auto"
  );
}

/** The conversation's preferences, defaulted to "Auto" where the hub omits them. */
export function turnPreferences(conversation: {
  readonly preferences?: TurnPreferences | undefined;
}): TurnPreferences {
  return conversation.preferences ?? DEFAULT_TURN_PREFERENCES;
}

// --- Decoders ---------------------------------------------------------------

/**
 * Whether anything can answer a chat in this project: a runner able to take
 * coordinator turns is enrolled, or a hub backend exists (decisions.md §1).
 * The per-project flag wins where the hub publishes one; otherwise the
 * organization-wide capability is all the client knows. It is never a reason to
 * block a send — a message queues and waits (decisions.md §9.1) — only a reason
 * to say so before the reader waits for an answer that cannot come.
 */
export function projectCoordinatorAvailable(
  bootstrap: Bootstrap,
  projectId: string,
): boolean {
  const project = bootstrap.projects.find((candidate) => candidate.id === projectId);
  return project?.coordinator ?? bootstrap.capabilities.coordinator;
}

/**
 * Deliveries a `retry` command is allowed on (decisions.md §10.3). Anything
 * else is `invalid_request`, so the affordance is not offered either.
 */
export const RETRYABLE_DELIVERIES: ReadonlySet<string> = new Set([
  "unknown",
  "failed",
  "rejected",
]);

/** True when this message can be re-queued by a `retry` command. */
export function isRetryableDelivery(delivery: string): boolean {
  return RETRYABLE_DELIVERIES.has(delivery);
}

export const decodeConversation = Schema.decodeUnknownSync(Conversation);
export const decodeMessage = Schema.decodeUnknownSync(Message);
export const decodeQuestion = Schema.decodeUnknownSync(Question);
export const decodeReceipt = Schema.decodeUnknownSync(Receipt);
export const decodeExecution = Schema.decodeUnknownSync(Execution);
export const decodeBootstrap = Schema.decodeUnknownSync(Bootstrap);
export const decodeConversationSnapshot = Schema.decodeUnknownSync(ConversationSnapshot);
export const decodeConversationList = Schema.decodeUnknownSync(ConversationListResponse);
export const decodeMessagePage = Schema.decodeUnknownSync(MessagePageResponse);
export const decodeCreateConversation = Schema.decodeUnknownSync(CreateConversationResponse);
export const decodeLinkResponse = Schema.decodeUnknownSync(LinkResponse);
export const decodeWorkItemReferences = Schema.decodeUnknownSync(WorkItemReferencesResponse);
export const decodeApiError = Schema.decodeUnknownSync(ApiError);
export const isApiError = Schema.is(ApiError);

/**
 * Decodes one SSE frame. The event type comes from the `event:` field, the
 * cursor from `id:`; only the `data:` body is validated against the schema.
 */
export function decodeEventFrame(type: string, data: unknown): ConversationEvent {
  return Schema.decodeUnknownSync(ConversationEvent)({ type, data });
}
