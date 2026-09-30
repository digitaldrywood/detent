import { ExternalLinkIcon } from "lucide-react";
import React from "react";

import type {
  AttentionItem,
  IssueProposal,
  IssueResult,
  Message,
} from "../../contracts/index.ts";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { cn } from "../../lib/utils.ts";
import { canRetryMessage } from "../../runtime/state/conversationState.ts";
import type { ExecutionSurface } from "../lib/execution.ts";
import type { TimelineCards } from "../adapters/timelineEntries.ts";
import { DeliveryChip } from "./DeliveryChip.tsx";
import { RETRY_COPY } from "./Timeline.tsx";
import { hubPath } from "../../runtime/basePath.ts";

const ATTENTION_TONE: Record<string, string> = {
  waiting_input: "bg-warning",
  failed: "bg-error",
  unknown: "bg-warning",
  running: "bg-success",
  starting: "bg-success",
  interrupting: "bg-warning",
  waiting_for_runner: "bg-warning",
};

/** What the seam needs to draw a card: the cards themselves and the actions. */
export interface TimelineCardsContextValue {
  /** Message id → the cards that message carries. */
  readonly cardsByMessageId: ReadonlyMap<string, TimelineCards>;
  /** Message id → the hub's own row, for the delivery footer below. */
  readonly messagesById: ReadonlyMap<string, Message>;
  readonly surface: ExecutionSurface;
  readonly onCreateIssue?: ((proposal: IssueProposal) => void) | undefined;
  readonly onOpenIssue?: ((workItemId: string) => void) | undefined;
  readonly onRetryMessage?: ((message: Message) => void) | undefined;
}

const EMPTY: TimelineCardsContextValue = {
  cardsByMessageId: new Map(),
  messagesById: new Map(),
  surface: "chat",
};

const TimelineCardsContext = React.createContext<TimelineCardsContextValue>(EMPTY);

export function TimelineCardsProvider({
  value,
  children,
}: {
  value: TimelineCardsContextValue;
  children: React.ReactNode;
}): React.ReactElement {
  return <TimelineCardsContext value={value}>{children}</TimelineCardsContext>;
}

function TranscriptCard({
  testId,
  children,
}: {
  testId: string;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <div
      className="my-2 w-full min-w-0 rounded-xl border border-border/70 bg-card/60 p-3 text-foreground shadow-xs"
      data-testid={testId}
    >
      {children}
    </div>
  );
}

function ProposalCard({
  proposal,
  text,
  onCreateIssue,
}: {
  proposal: IssueProposal;
  text: string;
  onCreateIssue?: ((proposal: IssueProposal) => void) | undefined;
}): React.ReactElement {
  return (
    <TranscriptCard testId="proposal-card">
      {text.length === 0 ? null : <p className="text-muted-foreground text-xs">{text}</p>}
      <h2 className="mt-1 font-medium text-sm">{proposal.title}</h2>
      <p className="mt-1 text-muted-foreground text-sm">{proposal.objective}</p>
      {onCreateIssue === undefined ? null : (
        <div className="mt-3 flex items-center gap-2">
          <Button size="sm" onClick={() => onCreateIssue(proposal)}>
            Create linked issue
          </Button>
        </div>
      )}
    </TranscriptCard>
  );
}

function IssueResultCard({
  issue,
  runnerBound,
  onOpenIssue,
}: {
  issue: IssueResult;
  runnerBound: boolean;
  onOpenIssue?: ((workItemId: string) => void) | undefined;
}): React.ReactElement {
  return (
    <TranscriptCard testId="issue-card">
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="font-mono text-muted-foreground text-xs tabular-nums">
          {issue.identifier}
        </span>
        <Badge variant="secondary" size="sm">
          {issue.lane ?? issue.state}
        </Badge>
        <Badge variant={runnerBound ? "success" : "warning"} size="sm">
          {runnerBound ? "Runner bound" : "Waiting for a runner"}
        </Badge>
      </div>
      <h2 className="mt-1.5 font-medium text-sm">{issue.title}</h2>
      {onOpenIssue === undefined ? null : (
        <div className="mt-3 flex items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => onOpenIssue(issue.id)}>
            Open issue
          </Button>
        </div>
      )}
    </TranscriptCard>
  );
}

function AttentionList({
  items,
  text,
  onOpenIssue,
}: {
  items: readonly AttentionItem[];
  text: string;
  onOpenIssue?: ((workItemId: string) => void) | undefined;
}): React.ReactElement {
  return (
    <TranscriptCard testId="attention-list">
      {text.length === 0 ? null : <p className="text-muted-foreground text-xs">{text}</p>}
      <ul className="m-0 mt-1.5 flex list-none flex-col gap-1 p-0">
        {items.map((item) => (
          <li key={item.work_item_id} className="flex min-w-0 items-center gap-2 text-sm">
            <a
              className="flex min-w-0 flex-1 items-center gap-2 rounded-md text-foreground no-underline hover:underline"
              href={hubPath(item.url ?? `/chat/issues/${item.work_item_id}`)}
              onClick={(event) => {
                if (onOpenIssue === undefined) return;
                if (event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0) return;
                event.preventDefault();
                onOpenIssue(item.work_item_id);
              }}
            >
              <span
                className={cn(
                  "size-1.5 shrink-0 rounded-full",
                  ATTENTION_TONE[item.state] ?? "bg-muted-foreground",
                )}
                aria-hidden="true"
              />
              <span className="min-w-0 truncate">{item.title}</span>
              <ExternalLinkIcon aria-hidden="true" className="size-3 shrink-0 opacity-50" />
            </a>
            <span className="shrink-0 text-muted-foreground text-xs">{item.reason}</span>
          </li>
        ))}
      </ul>
    </TranscriptCard>
  );
}

function DetentDeliveryFooter({
  message,
  surface,
  onRetryMessage,
}: {
  message: Message;
  surface: ExecutionSurface;
  onRetryMessage?: ((message: Message) => void) | undefined;
}): React.ReactElement | null {
  if (message.role !== "user") return null;
  const retryable = canRetryMessage(message) && onRetryMessage !== undefined;
  return (
    <div className="flex flex-col items-end gap-1">
      <div className="flex items-center gap-2 text-muted-foreground text-xs">
        <DeliveryChip status={message.delivery} surface={surface} />
        {retryable ? (
          <Button
            variant="ghost"
            size="xs"
            data-testid="message-retry"
            title={RETRY_COPY}
            onClick={() => onRetryMessage?.(message)}
          >
            Retry
          </Button>
        ) : null}
      </div>
      {retryable ? (
        <div
          className="rounded-md bg-warning-surface px-2 py-1 text-warning-foreground text-xs"
          role="status"
          data-testid="message-retry-note"
        >
          {RETRY_COPY}
        </div>
      ) : null}
    </div>
  );
}

export function DetentTimelineRow({
  messageId,
  children,
}: {
  messageId: string | null;
  children: React.ReactNode;
}): React.ReactElement {
  const { cardsByMessageId, messagesById, surface, onCreateIssue, onOpenIssue, onRetryMessage } =
    React.use(TimelineCardsContext);
  const cards = messageId === null ? undefined : cardsByMessageId.get(messageId);
  if (cards === undefined) {
    const message = messageId === null ? undefined : messagesById.get(messageId);
    return (
      <>
        {children}
        {message === undefined ? null : (
          <DetentDeliveryFooter
            message={message}
            surface={surface}
            onRetryMessage={onRetryMessage}
          />
        )}
      </>
    );
  }
  return (
    <>
      {cards.proposal === undefined ? null : (
        <ProposalCard
          proposal={cards.proposal}
          text={cards.text}
          onCreateIssue={onCreateIssue}
        />
      )}
      {cards.issue === undefined ? null : (
        <IssueResultCard
          issue={cards.issue}
          // The card reports what the issue itself says, not what the
          // conversation says: an issue can be created and still be waiting
          // for a runner to pick it up (decisions.md §9.2).
          runnerBound={cards.issue.runner_bound ?? false}
          onOpenIssue={onOpenIssue}
        />
      )}
      {cards.attention === undefined ? null : (
        <AttentionList items={cards.attention} text={cards.text} onOpenIssue={onOpenIssue} />
      )}
    </>
  );
}

export function detentRowTestId(row: {
  readonly kind: string;
  readonly message?: { readonly role: string };
}): { readonly "data-testid"?: string } {
  if (row.kind === "message") {
    return { "data-testid": row.message?.role === "user" ? "user-turn" : "assistant-turn" };
  }
  if (row.kind === "work" || row.kind === "work-live") return { "data-testid": "status-row" };
  if (row.kind === "turn-fold") return { "data-testid": "work-log" };
  return {};
}
