// Conversation detail state and its pure reducer.
//
// Written for this repository. The shape follows the POC's thread state
// (cached / synchronizing / live plus a page cursor) but the contents are the
// conversation resources from `docs/conversation/decisions.md` §5.
//
// Everything here is a pure function of (state, event). The atoms in
// `conversations.ts` own the effects; this file owns the merge rules, which
// are the part with the sharp edges: seq ordering, delta idempotence on
// replay, and optimistic messages that must never duplicate their accepted
// twin — nor silently disappear when their outcome is unknown.
import * as Option from "effect/Option";

import type {
  Answers,
  Conversation,
  ConversationEvent,
  ConversationSnapshot,
  Execution,
  Expected,
  Message,
  Question,
  Receipt,
} from "../../contracts/index.ts";
import { isRetryableDelivery } from "../../contracts/index.ts";

/**
 * The delivery ladder as a rank (decisions.md §5). It exists because two
 * writers report on the same command: the stream, which is ordered by seq, and
 * the HTTP response to the command itself, which is not ordered against it. A
 * slow `POST` returning `queued` after the stream already said `sent` must not
 * walk the ladder backwards — that would unlock a question card the runner has
 * already been answered on.
 */
const DELIVERY_RANK: Readonly<Record<string, number>> = {
  saved: 1,
  queued: 2,
  sending: 3,
  sent: 4,
  delivered: 5,
  responding: 6,
  // Terminal: nothing moves off these.
  completed: 7,
  interrupted: 7,
  rejected: 7,
  failed: 7,
  unknown: 7,
};

export function deliveryRank(status: string): number {
  return DELIVERY_RANK[status] ?? 0;
}

/** True when `next` is behind `current` on the ladder and must be ignored. */
export function isBackwardDelivery(current: string | null, next: string): boolean {
  if (current === null) return false;
  return deliveryRank(next) < deliveryRank(current);
}

export type ConversationStatus =
  | "empty"
  | "cached"
  | "synchronizing"
  | "live"
  | "gone";

/** Buffered `message.delta` text for one streaming message. */
export interface DeltaBuffer {
  /** Delta seq -> appended text. Re-delivering a seq overwrites it in place. */
  readonly parts: Readonly<Record<number, string>>;
}

export function deltaText(buffer: DeltaBuffer | undefined): string {
  if (buffer === undefined) return "";
  return Object.keys(buffer.parts)
    .map(Number)
    .sort((left, right) => left - right)
    .map((seq) => buffer.parts[seq] ?? "")
    .join("");
}

/**
 * A user message the client has sent but the hub has not confirmed. Keyed by
 * the command key, which is also the idempotency key.
 *
 * How it is retried depends on what the hub told us (decisions.md §10.3):
 * once a receipt named a message, the retry is a `retry` command carrying that
 * `messageId` and a new key, and the same message is re-queued. Until then no
 * message exists to name, so the only safe recovery is to re-send the byte
 * identical `message` command under the same key — which the hub either has
 * stored already or accepts for the first time. `expected` is part of that
 * payload, so it is kept: a re-send that dropped it would be a different
 * payload under the same key, which is `idempotency_conflict`.
 */
export interface PendingMessage {
  readonly key: string;
  readonly text: string;
  /**
   * The attachment ids this send carried (decisions.md §17.1), kept so a retry
   * resends the same command rather than a text-only version of it. Null for a
   * message with no files.
   */
  readonly attachments: readonly string[] | null;
  readonly createdAt: string;
  /** The message the hub created for this key, once a receipt named it. */
  readonly messageId: string | null;
  /**
   * The last delivery the hub reported for this key, verbatim. It is the
   * ladder position a later receipt is compared against; null until one
   * arrives.
   */
  readonly receiptStatus: string | null;
  /** The owner generation the original send carried, verbatim. */
  readonly expected: Expected | null;
  /** `unknown` is terminal until the user retries. It is never auto-retried. */
  readonly status: "sending" | "queued" | "unknown" | "failed";
  readonly error: string | null;
  /**
   * The error code that settled this entry, where one did. The code says more
   * than the ladder step it arrived on: `coordinator_unavailable` is a project
   * with no runner able to answer, not a message that merely failed.
   */
  readonly errorCode: string | null;
  /** True only where the hub said so, e.g. `503 queue_full`. */
  readonly retryable: boolean;
}

/** The controls this client sent that are not a message: U07's vocabulary. */
export type ControlKind = "answer" | "interrupt" | "continue";

/**
 * One control in the outbox. Like `PendingMessage` it is keyed by the command
 * key, so a retry reuses it and a question can never be answered twice by the
 * same intent. `unknown` is terminal until the user retries.
 */
export interface PendingControl {
  readonly key: string;
  readonly kind: ControlKind;
  /** The question an `answer` belongs to; null for the other kinds. */
  readonly questionId: string | null;
  /** The attempt the control was aimed at, as shown to the user when sent. */
  readonly attemptId: string | null;
  readonly createdAt: string;
  readonly status: "sending" | "queued" | "sent" | "unknown" | "rejected";
  readonly error: string | null;
  readonly errorCode: string | null;
  /** The last delivery the hub reported for this key. See `deliveryRank`. */
  readonly receiptStatus: string | null;
  /**
   * Everything a retry of this control needs, kept on the entry itself. It
   * lived in a component ref before, which meant navigating away and back left
   * the Retry button unable to rebuild the payload it had to resend byte for
   * byte.
   */
  readonly expected: Expected | null;
  readonly answers: Answers | null;
}

export interface ConversationPageState {
  readonly hasMore: boolean;
  readonly loadingOlder: boolean;
  /** Lowest loaded seq; the `before` value for the next older page. */
  readonly oldestSeq: number | null;
}

export interface ConversationDetail {
  readonly conversation: Conversation;
  /** Ordered by seq, ascending. Never re-ordered by a merge. */
  readonly messages: readonly Message[];
  readonly deltas: Readonly<Record<string, DeltaBuffer>>;
  readonly questions: readonly Question[];
  readonly receipts: Readonly<Record<string, Receipt>>;
  readonly pending: readonly PendingMessage[];
  /** Answers, interrupts and continues this client sent. */
  readonly controls: readonly PendingControl[];
  /**
   * True once a control came back `stale_execution`. The client says the
   * runner changed and re-reads the snapshot; it never re-aims the control at
   * whatever attempt is current now (decisions.md §2).
   */
  readonly staleExecution: boolean;
  readonly page: ConversationPageState;
}

export interface ConversationDetailState {
  readonly data: Option.Option<ConversationDetail>;
  readonly status: ConversationStatus;
  readonly error: Option.Option<string>;
  /** Set when the hub closed the stream and named a reason. */
  readonly closedReason: Option.Option<string>;
}

export const EMPTY_CONVERSATION_DETAIL_STATE: ConversationDetailState = {
  data: Option.none(),
  status: "empty",
  error: Option.none(),
  closedReason: Option.none(),
};

function bySeq(left: Message, right: Message): number {
  return left.seq - right.seq || left.id.localeCompare(right.id);
}

/** Inserts or replaces one message, keeping the list ordered by seq. */
export function upsertMessage(
  messages: readonly Message[],
  message: Message,
): readonly Message[] {
  const index = messages.findIndex((candidate) => candidate.id === message.id);
  if (index >= 0) {
    const next = messages.slice();
    next[index] = message;
    return next.toSorted(bySeq);
  }
  return [...messages, message].toSorted(bySeq);
}

function upsertQuestion(
  questions: readonly Question[],
  question: Question,
): readonly Question[] {
  const index = questions.findIndex((candidate) => candidate.id === question.id);
  if (index < 0) return [...questions, question];
  const next = questions.slice();
  next[index] = question;
  return next;
}

function appendDelta(
  deltas: Readonly<Record<string, DeltaBuffer>>,
  messageId: string,
  seq: number,
  text: string,
): Readonly<Record<string, DeltaBuffer>> {
  const existing = deltas[messageId];
  // Replaying the same delta writes the same value at the same key, so a
  // reconnect that re-delivers part of the window cannot double the text.
  if (existing !== undefined && existing.parts[seq] === text) return deltas;
  return {
    ...deltas,
    [messageId]: { parts: { ...(existing?.parts ?? {}), [seq]: text } },
  };
}

function withoutDelta(
  deltas: Readonly<Record<string, DeltaBuffer>>,
  messageId: string,
): Readonly<Record<string, DeltaBuffer>> {
  if (deltas[messageId] === undefined) return deltas;
  const next = { ...deltas };
  delete next[messageId];
  return next;
}

function dropPending(
  pending: readonly PendingMessage[],
  key: string | null,
): readonly PendingMessage[] {
  if (key === null) return pending;
  return pending.filter((entry) => entry.key !== key);
}

function receiptStatusToControl(receipt: Receipt): PendingControl["status"] {
  switch (receipt.status) {
    case "unknown":
      return "unknown";
    case "rejected":
      return "rejected";
    case "queued":
      return "queued";
    case "saved":
    case "sending":
      return "sending";
    case "sent":
    case "delivered":
      // An answer has no provider acknowledgement, so `sent` is as far as it
      // goes and is the success state the question card reports.
      return "sent";
  }
}

function receiptStatusToPending(receipt: Receipt): PendingMessage["status"] | "settled" {
  switch (receipt.status) {
    case "unknown":
      return "unknown";
    case "rejected":
      return "failed";
    case "queued":
      return "queued";
    case "saved":
    case "sending":
      return "sending";
    case "sent":
    case "delivered":
      return "settled";
  }
}

/** Builds the initial detail from a `GET /conversations/:id` snapshot. */
export function detailFromSnapshot(
  snapshot: ConversationSnapshot,
  previous?: ConversationDetail,
): ConversationDetail {
  const messages = snapshot.messages.slice().toSorted(bySeq);
  return {
    conversation: snapshot.conversation,
    messages,
    // A re-snapshot after `cursor_expired` replaces the transcript; buffered
    // deltas belonged to the expired window and would append twice.
    deltas: {},
    questions: snapshot.questions.slice(),
    receipts: previous?.receipts ?? {},
    // Optimistic entries survive a re-snapshot: their outcome is still the
    // client's to resolve, and dropping them would hide an unknown send.
    pending: (previous?.pending ?? []).filter(
      (entry) => !messages.some((message) => message.command_key === entry.key),
    ),
    // Controls are the same kind of client-owned outcome, and a `stale_execution`
    // notice has to outlive the refresh it triggers or the user never reads it.
    controls: previous?.controls ?? [],
    staleExecution: previous?.staleExecution ?? false,
    page: {
      hasMore: snapshot.has_more ?? messages.length > 0,
      loadingOlder: false,
      oldestSeq: messages.length > 0 ? (messages[0]?.seq ?? null) : null,
    },
  };
}

/** Applies one stream event. Returns the same object when nothing changed. */
export function applyConversationEvent(
  detail: ConversationDetail,
  event: ConversationEvent,
): ConversationDetail {
  switch (event.type) {
    case "conversation.updated":
      return { ...detail, conversation: event.data };
    case "execution.updated": {
      const execution: Execution = event.data;
      return {
        ...detail,
        conversation: { ...detail.conversation, execution },
      };
    }
    case "message.accepted": {
      const message: Message = event.data;
      return {
        ...detail,
        messages: upsertMessage(detail.messages, message),
        pending: dropPending(detail.pending, message.command_key),
        page: {
          ...detail.page,
          oldestSeq:
            detail.page.oldestSeq === null
              ? message.seq
              : Math.min(detail.page.oldestSeq, message.seq),
        },
      };
    }
    case "message.updated": {
      const message: Message = event.data;
      const known = detail.messages.some((candidate) => candidate.id === message.id);
      return {
        ...detail,
        messages: upsertMessage(detail.messages, message),
        // Once the final text lands the buffered deltas are redundant.
        deltas: message.text.length > 0 ? withoutDelta(detail.deltas, message.id) : detail.deltas,
        pending: dropPending(detail.pending, message.command_key),
        page: known
          ? detail.page
          : {
              ...detail.page,
              oldestSeq:
                detail.page.oldestSeq === null
                  ? message.seq
                  : Math.min(detail.page.oldestSeq, message.seq),
            },
      };
    }
    case "message.delta":
      return {
        ...detail,
        deltas: appendDelta(detail.deltas, event.data.message_id, event.data.seq, event.data.text),
      };
    case "question.opened":
    case "question.updated":
      return { ...detail, questions: upsertQuestion(detail.questions, event.data) };
    case "command.receipt": {
      const receipt: Receipt = event.data;
      const next = receiptStatusToPending(receipt);
      const controlStatus = receiptStatusToControl(receipt);
      const backwards = (entry: { readonly receiptStatus: string | null }) =>
        isBackwardDelivery(entry.receiptStatus, receipt.status);
      const settles =
        next === "settled" &&
        !detail.pending.some((entry) => entry.key === receipt.key && backwards(entry));
      return {
        ...detail,
        receipts: { ...detail.receipts, [receipt.key]: receipt },
        controls: detail.controls.map((entry) =>
          entry.key === receipt.key && !backwards(entry)
            ? {
                ...entry,
                status: controlStatus,
                receiptStatus: receipt.status,
                error: receipt.error?.message ?? null,
                errorCode: receipt.error?.code ?? null,
              }
            : entry,
        ),
        pending: settles
          ? dropPending(detail.pending, receipt.key)
          : detail.pending.map((entry) =>
              entry.key === receipt.key && !backwards(entry)
                ? {
                    ...entry,
                    // The receipt is where an optimistic entry learns which
                    // message it became: a retry needs that id to re-queue
                    // the message rather than send a second one.
                    messageId: receipt.message_id ?? entry.messageId,
                    receiptStatus: receipt.status,
                    status: next === "settled" ? entry.status : next,
                    error: receipt.error?.message ?? null,
                    errorCode: receipt.error?.code ?? null,
                    retryable: receipt.error?.code === "queue_full",
                  }
                : entry,
            ),
      };
    }
    case "heartbeat":
    case "closed":
      return detail;
  }
}

/** Merges an older page. Existing messages keep their position and identity. */
export function mergeOlderMessages(
  detail: ConversationDetail,
  older: readonly Message[],
  hasMore: boolean,
): ConversationDetail {
  const known = new Set(detail.messages.map((message) => message.id));
  const added = older.filter((message) => !known.has(message.id));
  const messages = added.length === 0 ? detail.messages : [...added, ...detail.messages].toSorted(bySeq);
  return {
    ...detail,
    messages,
    page: {
      hasMore,
      loadingOlder: false,
      oldestSeq: messages.length > 0 ? (messages[0]?.seq ?? null) : detail.page.oldestSeq,
    },
  };
}

export function withPending(
  detail: ConversationDetail,
  entry: PendingMessage,
): ConversationDetail {
  const index = detail.pending.findIndex((candidate) => candidate.key === entry.key);
  if (index < 0) return { ...detail, pending: [...detail.pending, entry] };
  const pending = detail.pending.slice();
  pending[index] = entry;
  return { ...detail, pending };
}

export function withoutPending(detail: ConversationDetail, key: string): ConversationDetail {
  return { ...detail, pending: dropPending(detail.pending, key) };
}

/** Adds or replaces one control outbox entry, keyed by its command key. */
export function withControl(
  detail: ConversationDetail,
  entry: PendingControl,
): ConversationDetail {
  const index = detail.controls.findIndex((candidate) => candidate.key === entry.key);
  if (index < 0) return { ...detail, controls: [...detail.controls, entry] };
  const controls = detail.controls.slice();
  controls[index] = entry;
  return { ...detail, controls };
}

export function withoutControl(detail: ConversationDetail, key: string): ConversationDetail {
  return { ...detail, controls: detail.controls.filter((entry) => entry.key !== key) };
}

export function withStaleExecution(
  detail: ConversationDetail,
  stale: boolean,
): ConversationDetail {
  return detail.staleExecution === stale ? detail : { ...detail, staleExecution: stale };
}

/** The most recent control of a kind, optionally scoped to one question. */
export function latestControl(
  detail: ConversationDetail,
  kind: ControlKind,
  questionId?: string,
): PendingControl | undefined {
  let found: PendingControl | undefined;
  for (const entry of detail.controls) {
    if (entry.kind !== kind) continue;
    if (questionId !== undefined && entry.questionId !== questionId) continue;
    found = entry;
  }
  return found;
}

/** Outbox entries the user can still act on: nothing here is auto-retried. */
export function unsettledControls(detail: ConversationDetail): readonly PendingControl[] {
  return detail.controls.filter(
    (entry) => entry.status === "unknown" || entry.status === "queued",
  );
}

/**
 * A question is closed to this client when the hub says so, when it expired,
 * or when this client's own answer was accepted. `rejected` for
 * `question_already_answered` closes it too: someone else won.
 */
export function questionLocked(
  question: Question,
  control: PendingControl | undefined,
): boolean {
  if (question.status === "answered" || question.status === "expired") return true;
  if (control === undefined) return false;
  if (control.status === "sent") return true;
  return control.status === "rejected" && control.errorCode === "question_already_answered";
}

/**
 * Questions anchored to one message. An answered question keeps its place so
 * a second tab reads the resolution instead of answering again (U07).
 */
export function questionsForMessage(
  detail: ConversationDetail,
  messageId: string,
): readonly Question[] {
  return detail.questions.filter((question) => question.message_id === messageId);
}

/** Questions whose anchor message is not in the loaded window. */
export function unanchoredQuestions(detail: ConversationDetail): readonly Question[] {
  const known = new Set(detail.messages.map((message) => message.id));
  return detail.questions.filter((question) => !known.has(question.message_id));
}

/**
 * User messages the hub persisted whose delivery the user can still act on
 * (decisions.md §10.3). A `retry` command is refused on anything else, so the
 * affordance is not offered on anything else either.
 */
export function retryableMessages(detail: ConversationDetail): readonly Message[] {
  return detail.messages.filter(
    (message) => message.role === "user" && isRetryableDelivery(message.delivery),
  );
}

/** True when this persisted message can be re-queued by a `retry` command. */
export function canRetryMessage(message: Message): boolean {
  return message.role === "user" && isRetryableDelivery(message.delivery);
}

/** True while the coordinator or runner is producing output. */
export function isStreaming(detail: ConversationDetail): boolean {
  const status = detail.conversation.execution.status;
  return status === "running" || status === "starting" || Object.keys(detail.deltas).length > 0;
}
