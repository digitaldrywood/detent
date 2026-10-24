import type { Conversation } from "../../contracts/index.ts";

export type SidebarGroup = "active" | "settled";

/** A conversation is settled once nothing is running and a day has passed. */
export const SETTLED_AFTER_MS = 24 * 60 * 60 * 1_000;

const LIVE_EXECUTION_STATUSES = new Set([
  "starting",
  "running",
  "waiting_input",
  "waiting_for_runner",
  "interrupting",
]);

/**
 * First VALID timestamp wins: `a ?? b` falls through on null, but a present-
 * yet-malformed string must also fall through to the next candidate rather
 * than sink the row to the epoch. (Upstream `firstValidTimestampMs`.)
 */
export function firstValidTimestampMs(
  ...candidates: ReadonlyArray<string | null | undefined>
): number {
  for (const candidate of candidates) {
    if (candidate == null) continue;
    const parsed = Date.parse(candidate);
    if (!Number.isNaN(parsed)) return parsed;
  }
  return 0;
}

export function conversationActivityMs(conversation: Conversation): number {
  return firstValidTimestampMs(
    conversation.last_message_at,
    conversation.execution.updated_at,
    conversation.updated_at,
    conversation.created_at,
  );
}

/**
 * Which shelf a conversation belongs on (decisions.md §13.9, §14). The status
 * is the authority: `settled` — and `archived`, which is the same thing under
 * the name the hub has not dropped yet — is settled, full stop.
 *
 * The recency rule below it is the client's stand-in until the hub settles
 * conversations itself. §14 gives the hub the same rule and the same default
 * window, so the shelf reads the same either way, and the moment a hub starts
 * sending `settled` the stand-in simply stops being the thing that decides.
 */
export function conversationGroup(
  conversation: Conversation,
  options: { readonly now: number },
): SidebarGroup {
  if (conversation.status !== "active") return "settled";
  if (LIVE_EXECUTION_STATUSES.has(conversation.execution.status)) return "active";
  return options.now - conversationActivityMs(conversation) > SETTLED_AFTER_MS
    ? "settled"
    : "active";
}

/** Most recent activity first; ties break on id so the order never flickers. */
export function sortConversations(
  conversations: readonly Conversation[],
): readonly Conversation[] {
  return conversations
    .slice()
    .toSorted(
      (left, right) =>
        conversationActivityMs(right) - conversationActivityMs(left) ||
        left.id.localeCompare(right.id),
    );
}

export interface SidebarGroups {
  readonly active: readonly Conversation[];
  readonly settled: readonly Conversation[];
}

export function groupConversations(
  conversations: readonly Conversation[],
  options: { readonly now: number; readonly projectId?: string | null },
): SidebarGroups {
  const scoped =
    options.projectId == null
      ? conversations
      : conversations.filter((conversation) => conversation.project_id === options.projectId);
  const sorted = sortConversations(scoped);
  return {
    active: sorted.filter((conversation) => conversationGroup(conversation, options) === "active"),
    settled: sorted.filter(
      (conversation) => conversationGroup(conversation, options) === "settled",
    ),
  };
}

/**
 * Search the already-ordered collection by title only. Keeping the input order
 * means lifecycle ordering stays stable while the user narrows the list.
 * (Upstream `searchSidebarThreadsByTitle`.)
 */
export function searchConversationsByTitle(
  conversations: readonly Conversation[],
  query: string,
): readonly Conversation[] {
  const normalized = query.trim().toLowerCase();
  if (normalized.length === 0) return [];
  return conversations.filter((conversation) =>
    conversation.title.toLowerCase().includes(normalized),
  );
}

/** Below this the client filters locally; at or above it the hub is asked. */
export const SERVER_SEARCH_MIN_LENGTH = 2;

export function shouldSearchServer(query: string): boolean {
  return query.trim().length >= SERVER_SEARCH_MIN_LENGTH;
}

/**
 * Shift-click creates directly in the current project, skipping the picker.
 * With a single project there is nothing to pick, so a plain click already
 * creates immediately. (Upstream `shouldCreateNewThreadInCurrentProject`.)
 */
export function shouldCreateInCurrentProject(shiftKey: boolean, projectCount: number): boolean {
  return shiftKey || projectCount <= 1;
}

/**
 * Compact relative label for the sidebar's right column. Minutes round down;
 * anything under a minute reads "now" rather than "0m".
 */
export function relativeTimeLabel(iso: string | null, options: { readonly now: number }): string {
  const at = firstValidTimestampMs(iso);
  if (at === 0) return "";
  const elapsed = options.now - at;
  if (elapsed < 60_000) return "now";
  if (elapsed < 60 * 60_000) return `${Math.floor(elapsed / 60_000)}m`;
  if (elapsed < 24 * 60 * 60_000) return `${Math.floor(elapsed / (60 * 60_000))}h`;
  if (elapsed < 7 * 24 * 60 * 60_000) return `${Math.floor(elapsed / (24 * 60 * 60_000))}d`;
  return `${Math.floor(elapsed / (7 * 24 * 60 * 60_000))}w`;
}

export type RowSignal =
  | { readonly kind: "streaming" }
  | { readonly kind: "attention" }
  | { readonly kind: "time"; readonly label: string };

/**
 * Exactly one signal per row, in priority order: a live turn, then an
 * unanswered question, then the relative time. Never two at once.
 */
export function rowSignal(
  conversation: Conversation,
  options: { readonly now: number; readonly needsAttention: boolean },
): RowSignal {
  const status = conversation.execution.status;
  if (status === "running" || status === "starting") return { kind: "streaming" };
  if (options.needsAttention || status === "waiting_input") return { kind: "attention" };
  return {
    kind: "time",
    label: relativeTimeLabel(conversation.last_message_at ?? conversation.updated_at, options),
  };
}
