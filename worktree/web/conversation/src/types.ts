import { videoMimeType } from "@t3tools/shared/video";

import type {
  EnvironmentProject,
  EnvironmentThread,
  EnvironmentThreadShell,
} from "./app/adapters/models.ts";
import type {
  ChatFileAttachment as ContractChatFileAttachment,
  ChatImageAttachment as ContractChatImageAttachment,
  ChatUnknownAttachment as ContractChatUnknownAttachment,
  OrchestrationCheckpointFile,
  OrchestrationCheckpointSummary,
  OrchestrationLatestTurn,
  OrchestrationMessage,
  OrchestrationProposedPlan,
  OrchestrationSession,
  ProjectScript as ContractProjectScript,
} from "./contracts/ui.ts";

export { videoMimeType } from "@t3tools/shared/video";

export type ProjectScript = ContractProjectScript;

/** Upstream's, verbatim. */
export interface ChatImageAttachment extends ContractChatImageAttachment {
  readonly previewUrl?: string;
}

/** Upstream's, verbatim. */
export interface ChatFileAttachment extends ContractChatFileAttachment {
  readonly previewUrl?: string;
  readonly downloadable?: boolean;
}

// Attachment types this build does not know pass through with the contract
// shape. The UI renders them as inert rows so a newer server cannot crash an
// older client.
export type ChatUnknownAttachment = ContractChatUnknownAttachment;

export type ChatAttachment = ChatImageAttachment | ChatFileAttachment | ChatUnknownAttachment;

// The union has an open member (`type: string`), so a literal comparison does
// not narrow. Use these guards wherever type-specific fields are read.
export function isImageAttachment(attachment: ChatAttachment): attachment is ChatImageAttachment {
  return attachment.type === "image";
}

export function isFileAttachment(attachment: ChatAttachment): attachment is ChatFileAttachment {
  return attachment.type === "file";
}

export function isVideoAttachment(attachment: ChatFileAttachment): boolean {
  return videoMimeType(attachment) !== null;
}

export function isBrowserPreviewAttachment(attachment: ChatFileAttachment): boolean {
  const mimeType = attachment.mimeType.split(";", 1)[0]?.trim().toLowerCase();
  return (
    /\.(?:html?|pdf)$/i.test(attachment.name) ||
    mimeType === "application/pdf" ||
    mimeType === "text/html"
  );
}

/** Upstream's, verbatim. */
export interface ChatMessage extends Omit<OrchestrationMessage, "attachments"> {
  readonly attachments?: ReadonlyArray<ChatAttachment> | undefined;
}

export type ProposedPlan = OrchestrationProposedPlan;
export type TurnDiffFileChange = OrchestrationCheckpointFile;
export type TurnDiffSummary = OrchestrationCheckpointSummary;

export type SessionPhase = "disconnected" | "connecting" | "ready" | "running";

export type Project = EnvironmentProject;
export type Thread = EnvironmentThread;
export type ThreadShell = EnvironmentThreadShell;

export interface ThreadTurnState {
  latestTurn: OrchestrationLatestTurn | null;
}

export type SidebarThreadSummary = EnvironmentThreadShell;
export type ThreadSession = OrchestrationSession;
