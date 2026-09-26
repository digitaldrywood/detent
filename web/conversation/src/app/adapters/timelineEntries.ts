import {
  type AttentionItem,
  type IssueProposal,
  type IssueResult,
  type Message,
  type MessageAttachment,
  readAttention,
  readIssueProposal,
  readIssueResult,
} from "../../contracts/index.ts";
import type { ChatAttachment, ChatMessage, ProposedPlan } from "../../types.ts";
import type { WorkLogEntry } from "../../session-logic.ts";
import { deltaText, type ConversationDetail } from "../../runtime/state/conversationState.ts";

/** The three cards a status or tool message can carry. */
export interface TimelineCards {
  readonly proposal?: IssueProposal;
  readonly issue?: IssueResult;
  readonly attention?: readonly AttentionItem[];
  /** The message's own prose, which the card renders as its preamble. */
  readonly text: string;
}

/** The cards on one message, or `undefined` when it carries none. */
export function readTimelineCards(message: Message): TimelineCards | undefined {
  if (message.kind !== "status" && message.kind !== "tool") return undefined;
  const proposal = readIssueProposal(message.data);
  const issue = readIssueResult(message.data);
  const attention = readAttention(message.data);
  if (proposal === undefined && issue === undefined && attention.length === 0) return undefined;
  return {
    ...(proposal === undefined ? {} : { proposal }),
    ...(issue === undefined ? {} : { issue }),
    ...(attention.length === 0 ? {} : { attention }),
    text: message.text,
  };
}

/** True for a status or tool message that folds into the work log. */
export function isWorkMessage(message: Message): boolean {
  return (
    (message.kind === "status" || message.kind === "tool") &&
    readTimelineCards(message) === undefined
  );
}

export function toChatAttachment(attachment: MessageAttachment): ChatAttachment {
  const shared = {
    id: attachment.id,
    name: attachment.name,
    mimeType: attachment.mime,
    sizeBytes: attachment.size,
    previewUrl: attachment.url,
  };
  return attachment.mime.toLowerCase().startsWith("image/")
    ? { ...shared, type: "image" as const }
    : { ...shared, type: "file" as const, downloadable: true };
}

export function toChatMessage(message: Message, streamingText: string): ChatMessage {
  const cards = readTimelineCards(message);
  const attachments = message.attachments ?? [];
  return {
    id: message.id,

    role: message.role === "user" ? "user" : "assistant",
    // A card owns its own prose, so the spoken body is empty and the copied
    // row draws the card alone.
    text: cards === undefined ? `${message.text}${streamingText}` : "",
    ...(attachments.length === 0
      ? {}
      : { attachments: attachments.map(toChatAttachment) }),
    turnId: message.turn_id,
    streaming: streamingText.length > 0 || message.delivery === "responding",
    createdAt: message.created_at,
    updatedAt: message.updated_at,
  };
}

export function toWorkLogEntry(message: Message): WorkLogEntry {
  const failed = message.delivery === "failed";
  return {
    id: message.id,
    createdAt: message.created_at,
    turnId: message.turn_id,
    label: message.text.length > 0 ? message.text : (message.command_key ?? "Working"),
    tone: failed ? "error" : message.kind === "tool" ? "tool" : "info",
    // The copied `workEntrySignalsSevereFailure` reads this to decide whether
    // a row keeps the severe-failure treatment rather than an ordinary
    // nonzero exit, so a failed delivery is reported as a `.failed` kind.
    sourceActivityKind: failed ? "message.failed" : `message.${message.kind}`,
    ...(message.command_key === null ? {} : { toolTitle: message.command_key }),
  };
}

export interface TimelineSources {
  readonly messages: ReadonlyArray<ChatMessage>;
  readonly proposedPlans: ReadonlyArray<ProposedPlan>;
  readonly workEntries: ReadonlyArray<WorkLogEntry>;
}

export function timelineSources(detail: ConversationDetail): TimelineSources {
  const messages: ChatMessage[] = [];
  const workEntries: WorkLogEntry[] = [];
  for (const message of detail.messages) {
    if (isWorkMessage(message)) {
      workEntries.push(toWorkLogEntry(message));
      continue;
    }
    messages.push(toChatMessage(message, deltaText(detail.deltas[message.id])));
  }
  return { messages, proposedPlans: EMPTY_PROPOSED_PLANS, workEntries };
}

const EMPTY_PROPOSED_PLANS: ReadonlyArray<ProposedPlan> = [];
