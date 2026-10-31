import { routerBasePath } from "../../runtime/basePath.ts";

export function conversationWebURL(conversationId: string, projectId: string): string {
  const prefix = routerBasePath().replace(/\/$/, "");
  const url = new URL(`${prefix}/chat/c/${encodeURIComponent(conversationId)}`, globalThis.location.origin);
  url.searchParams.set("project_id", projectId);
  return url.href;
}

export function conversationHandoff(identity: {
  readonly organization_id: string;
  readonly project_id: string;
  readonly conversation_id: string;
  readonly web_url: string;
}): string {
  return [
    "Read this Detent conversation through the authorized MCP connection:",
    JSON.stringify(identity, null, 2),
    "Use get_conversation with project_id and conversation_id. Read older messages with list_conversation_messages, passing the numeric next_cursor as before until next_cursor is null. Read permitted message attachments with get_conversation_attachment, using attachment_id and next_offset while has_more is true.",
    "For discovery, use list_project_conversations; subject_work_item_id filters private subject chats visible to your identity. An empty list does not prove another user has no private chat.",
    "This link grants no access. Report access_denied or operator access is unavailable as a permission limitation. Read only; do not post messages, dispatch work or change issue state.",
  ].join("\n\n");
}
