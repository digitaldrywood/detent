// Renders conversation messages and their pending questions and media previews.
import { LegendListRef } from "@legendapp/list/react";
import React from "react";

import type { IssueProposal, Message, Question } from "../../contracts/index.ts";
import { HUB_ENVIRONMENT_ID } from "../../contracts/index.ts";
import { MessagesTimeline } from "../../components/chat/MessagesTimeline.tsx";
import { ExpandedImageDialog } from "../../components/chat/ExpandedImageDialog.tsx";
import type { ExpandedImagePreview } from "../../components/chat/ExpandedImagePreview.tsx";
import { Button } from "../../components/ui/button.tsx";
import { deriveTimelineEntries } from "../../session-logic.ts";
import {
  type ConversationDetail,
  type PendingMessage,
  questionsForMessage,
  unanchoredQuestions,
} from "../../runtime/state/conversationState.ts";
import { useTheme } from "../adapters/theme.ts";
import { readTimelineCards, timelineSources } from "../adapters/timelineEntries.ts";
import { TimelineCardsProvider } from "./TimelineCards.tsx";
import { PendingRow } from "./PendingRow.tsx";
import type { ExecutionSurface } from "../lib/execution.ts";

/** The retry promise, in one sentence, wherever a retry is offered (§10.3). */
export const RETRY_COPY = "Retry re-queues this message once; nothing is duplicated.";

export interface TimelineProps {
  readonly detail: ConversationDetail;
  readonly onLoadOlder: () => void;
  readonly onRetry: (entry: PendingMessage) => void;
  /** Re-queues a message the hub already stored (decisions.md §10.3). */
  readonly onRetryMessage?: (message: Message) => void;
  readonly onDismiss: (entry: PendingMessage) => void;
  /** Opens the handoff form prefilled from a coordinator proposal (U06). */
  readonly onCreateIssue?: (proposal: IssueProposal) => void;
  /** Navigates to `/chat/issues/<work item>` (U06 result card, U08 links). */
  readonly onOpenIssue?: (workItemId: string) => void;
  /** Questions render anchored to their message; the shell wires the card. */
  readonly renderQuestion?: (question: Question) => React.ReactNode;

  readonly onIsAtEndChange?: (isAtEnd: boolean) => void;
  /** The shell's handle on the list, for scrolling it back to the live edge. */
  readonly listRef?: React.RefObject<LegendListRef | null>;
}

/**
 * Announces the end of a streamed reply. The transcript's live region is
 * `aria-relevant="additions"`, so a reply that grows character by character is
 * never announced — announcing every delta would be unusable anyway. What a
 * screen-reader user needs is the moment the reply is finished and can be read.
 */
function useReplyAnnouncement(detail: ConversationDetail): string {
  const streaming =
    Object.keys(detail.deltas).length > 0 ||
    detail.messages.some((message) => message.delivery === "responding");
  const [announcement, setAnnouncement] = React.useState("");
  const wasStreaming = React.useRef(streaming);
  React.useEffect(() => {
    if (wasStreaming.current && !streaming) setAnnouncement("Reply finished");
    if (!wasStreaming.current && streaming) setAnnouncement("");
    wasStreaming.current = streaming;
  }, [streaming]);
  return announcement;
}

const NO_TURN_DIFF_SUMMARIES: ReadonlyArray<never> = [];

export function Timeline(props: TimelineProps): React.ReactElement {
  const { detail } = props;
  const renderQuestion = props.renderQuestion;
  const announcement = useReplyAnnouncement(detail);
  const { resolvedTheme } = useTheme();
  const ownListRef = React.useRef<LegendListRef | null>(null);
  const listRef = props.listRef ?? ownListRef;
  const [expandedImage, setExpandedImage] = React.useState<ExpandedImagePreview | null>(null);

  // The transcript reads its own surface off the conversation: an unlinked chat
  // is answered by a coordinator turn, so its delivery vocabulary is the chat
  // one and nothing above has to remember to say so.
  const surface: ExecutionSurface = detail.conversation.work_item_id === null ? "chat" : "issue";

  const sources = React.useMemo(() => timelineSources(detail), [detail]);
  const timelineEntries = React.useMemo(
    () => deriveTimelineEntries(sources.messages, sources.proposedPlans, sources.workEntries),
    [sources],
  );

  const cardsByMessageId = React.useMemo(() => {
    const cards = new Map<string, ReturnType<typeof readTimelineCards>>();
    for (const message of detail.messages) {
      const found = readTimelineCards(message);
      if (found !== undefined) cards.set(message.id, found);
    }
    return cards as ReadonlyMap<string, NonNullable<ReturnType<typeof readTimelineCards>>>;
  }, [detail.messages]);

  const isWorking = detail.messages.some((message) => message.delivery === "responding");
  const activeTurnStartedAt =
    detail.messages.find((message) => message.delivery === "responding")?.created_at ?? null;

  const messagesById = React.useMemo(
    () => new Map(detail.messages.map((message) => [message.id, message])),
    [detail.messages],
  );

  const cardContext = React.useMemo(
    () => ({
      cardsByMessageId,
      messagesById,
      surface,
      onCreateIssue: props.onCreateIssue,
      onOpenIssue: props.onOpenIssue,
      onRetryMessage: props.onRetryMessage,
    }),
    [
      cardsByMessageId,
      messagesById,
      surface,
      props.onCreateIssue,
      props.onOpenIssue,
      props.onRetryMessage,
    ],
  );

  /** Questions anchored to `messageId`, rendered where that message renders. */
  const questions = (messageId: string) =>
    renderQuestion === undefined
      ? null
      : questionsForMessage(detail, messageId).map((question) => (
          <React.Fragment key={question.id}>{renderQuestion(question)}</React.Fragment>
        ));

  return (
    <div className="flex min-h-0 w-full flex-1 flex-col">
      {detail.page.hasMore ? (
        <div className="flex justify-center py-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={props.onLoadOlder}
            disabled={detail.page.loadingOlder}
          >
            {detail.page.loadingOlder ? "Loading earlier messages" : "Load earlier messages"}
          </Button>
        </div>
      ) : null}

      <div className="min-h-0 flex-1" aria-live="polite" aria-relevant="additions">
        <TimelineCardsProvider value={cardContext}>
          <MessagesTimeline
            isWorking={isWorking}
            activeTurnStartedAt={activeTurnStartedAt}
            listRef={listRef}
            timelineEntries={timelineEntries}
            latestTurn={null}
            runningTurnId={null}
            turnDiffSummaries={NO_TURN_DIFF_SUMMARIES}
            routeThreadKey={`${HUB_ENVIRONMENT_ID}:${detail.conversation.id}`}
            onOpenTurnDiff={NOOP_OPEN_TURN_DIFF}
            supportsConversationRollback={false}
            onRevertToTurnCount={NOOP_REVERT}
            isRevertingCheckpoint={false}
            onImageExpand={setExpandedImage}
            activeThreadEnvironmentId={HUB_ENVIRONMENT_ID}
            markdownCwd={undefined}
            resolvedTheme={resolvedTheme}
            timestampFormat="locale"
            workspaceRoot={undefined}
            anchorMessageId={null}
            onAnchorReady={NOOP_ANCHOR_READY}
            contentInsetEndAdjustment={0}
            liveFollowEnabled
            onIsAtEndChange={props.onIsAtEndChange ?? NOOP_IS_AT_END}
            onManualNavigation={NOOP_MANUAL_NAVIGATION}
          />
        </TimelineCardsProvider>
      </div>

      <div className="mx-auto flex w-full max-w-3xl flex-col gap-2 px-3 sm:px-4">
        {renderQuestion === undefined
          ? null
          : detail.messages.map((message) => (
              <React.Fragment key={message.id}>{questions(message.id)}</React.Fragment>
            ))}
        {renderQuestion === undefined
          ? null
          : unanchoredQuestions(detail).map((question) => (
              <React.Fragment key={question.id}>{renderQuestion(question)}</React.Fragment>
            ))}

        {detail.pending.map((entry) => (
          <PendingRow
            key={entry.key}
            entry={entry}
            surface={surface}
            onRetry={props.onRetry}
            onDismiss={props.onDismiss}
          />
        ))}
      </div>

      <p className="dc-sr-only" role="status" data-testid="reply-announcement">
        {announcement}
      </p>

      {expandedImage === null ? null : (
        <ExpandedImageDialog preview={expandedImage} onClose={() => setExpandedImage(null)} />
      )}
    </div>
  );
}

const NOOP_OPEN_TURN_DIFF = () => {};
const NOOP_REVERT = () => {};
const NOOP_ANCHOR_READY = () => {};
const NOOP_IS_AT_END = () => {};
const NOOP_MANUAL_NAVIGATION = () => {};
