import {
  CircleDotIcon,
  FileDiffIcon,
  type LucideIcon,
  MessageSquareIcon,
  MessageSquareQuoteIcon,
  PaperclipIcon,
  PencilIcon,
  PlusIcon,
  SendHorizonalIcon,
  SquareIcon,
  TimerResetIcon,
  TriangleAlertIcon,
  WorkflowIcon,
} from "lucide-react";
import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import {
  Collapsible,
  CollapsiblePanel,
  CollapsibleTrigger,
} from "../../../components/ui/collapsible.tsx";
import { cn } from "../../../lib/utils.ts";
import { Markdown } from "../../components/Markdown.tsx";
import {
  type ActivityGroup,
  type ActivityIcon,
  type ActivityRow,
  foldActivity,
  foldLabel,
  timeLabel,
} from "../lib/activity.ts";

const ICONS: Record<ActivityIcon, LucideIcon> = {
  created: PlusIcon,
  moved: WorkflowIcon,
  edited: PencilIcon,
  labelled: PencilIcon,
  related: CircleDotIcon,
  runner: TimerResetIcon,
  diff: FileDiffIcon,
  comment: MessageSquareIcon,
  question: MessageSquareQuoteIcon,
  steering: SendHorizonalIcon,
  attachment: PaperclipIcon,
  stopped: SquareIcon,
  failed: TriangleAlertIcon,
};

function RowGlyph({ icon }: { readonly icon: ActivityIcon }): React.ReactElement {
  const Icon = ICONS[icon];
  return (
    <span
      aria-hidden
      className="flex size-[22px] shrink-0 items-center justify-center rounded-full border border-border bg-muted text-muted-foreground"
    >
      <Icon className="size-3" />
    </span>
  );
}

function EventRow({ row }: { readonly row: ActivityRow }): React.ReactElement {
  return (
    <li
      data-testid="issue-activity-row"
      data-activity-icon={row.icon}
      className="flex items-center gap-3 py-1 text-[13px]"
    >
      <RowGlyph icon={row.icon} />
      <p className="min-w-0 text-muted-foreground">
        <span className="font-medium text-foreground">{row.actor}</span> {row.sentence}
        {timeLabel(row.at) === "" ? null : (
          <span className="text-muted-foreground/70"> · {timeLabel(row.at)}</span>
        )}
      </p>
    </li>
  );
}

/**
 * A comment, as its own card with the reply attached to it.
 *
 * The reply is a comment on the same issue, not a threaded child: the hub's
 * comments have no parent, so the footer says "Leave a reply" and posts one
 * more comment rather than pretending to a thread the API cannot store.
 */
function CommentCard({
  row,
  onReply,
  posting,
}: {
  readonly row: ActivityRow;
  readonly onReply: ((body: string) => void) | null;
  readonly posting: boolean;
}): React.ReactElement {
  const [draft, setDraft] = React.useState("");
  const comment = row.comment;
  return (
    <li className="my-1.5" data-testid="issue-comment">
      <article className="rounded-[var(--radius)] border border-border bg-card">
        <div className="px-3.5 py-3">
          <div className="flex items-center gap-2 text-[13px]">
            <span className="font-medium text-foreground">{row.actor}</span>
            <span className="text-muted-foreground/70">{timeLabel(row.at)}</span>
          </div>
          <div className="mt-2 text-sm" data-testid="issue-comment-body">
            <Markdown source={comment?.body ?? ""} />
          </div>
        </div>
        {onReply === null ? null : (
          <form
            className="flex items-center gap-2 border-border border-t px-3 py-2"
            onSubmit={(event) => {
              event.preventDefault();
              if (draft.trim().length === 0 || posting) return;
              onReply(draft.trim());
              setDraft("");
            }}
          >
            <input
              value={draft}
              onChange={(event) => setDraft(event.target.value)}
              data-testid="issue-comment-reply"
              aria-label={`Reply to ${row.actor}`}
              placeholder="Leave a reply…"
              className="min-w-0 flex-1 bg-transparent text-[13px] text-foreground outline-none placeholder:text-muted-foreground/70"
            />
            <Button
              type="submit"
              size="xs"
              variant="outline"
              disabled={draft.trim().length === 0 || posting}
            >
              Reply
            </Button>
          </form>
        )}
      </article>
    </li>
  );
}

function FoldRow({ rows }: { readonly rows: readonly ActivityRow[] }): React.ReactElement {
  return (
    <li className="py-0.5">
      <Collapsible>
        <CollapsibleTrigger
          data-testid="activity-fold"
          className="cursor-pointer rounded-sm px-0 text-left text-[13px] text-muted-foreground outline-none ring-ring hover:text-foreground focus-visible:ring-2"
        >
          {foldLabel(rows)}
        </CollapsibleTrigger>
        <CollapsiblePanel>
          <ol className="mt-1 border-border border-l ps-3">
            {rows.map((row) => (
              <EventRow key={row.key} row={row} />
            ))}
          </ol>
        </CollapsiblePanel>
      </Collapsible>
    </li>
  );
}

export interface LiveRowProps {
  /** True while an attempt holds the issue. */
  readonly running: boolean;
  /** The runner's current sentence, or the last turn's summary. */
  readonly sentence: string;
  /** `Preview runner · Codex gpt-6-astra · attempt 2`, where it is known. */
  readonly detail: string;
  readonly elapsed: string;
  /** Absent when nothing can be interrupted. */
  readonly onInterrupt: (() => void) | null;
  readonly onOpenConversation: () => void;
}

/**
 * The conversation, as one row.
 *
 * While an attempt runs it pulses — the same `animate-status-pulse` dot the
 * board's live card wears — and carries Interrupt; when nothing runs it is
 * the last turn's one-line summary with the same link. Either way the whole
 * row opens the conversation in the right panel.
 */
export function LiveRow(props: LiveRowProps): React.ReactElement {
  return (
    <li data-testid="issue-live-row" data-live={props.running ? "true" : "false"} className="my-1.5">
      <div
        className={cn(
          "flex items-start gap-3 rounded-[var(--radius)] border px-3.5 py-3",
          props.running ? "border-success/35 bg-success/[0.05]" : "border-border bg-card",
        )}
      >
        <span className="flex size-[22px] shrink-0 items-center justify-center">
          <span
            aria-hidden
            className={cn(
              "size-2.5 rounded-full",
              props.running
                ? "bg-success motion-safe:animate-status-pulse"
                : "bg-muted-foreground/40",
            )}
          />
        </span>
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <div className="flex items-center gap-2 text-[13px]">
            <span className="font-medium text-foreground" data-testid="live-row-title">
              {props.running ? "The runner is working" : "Conversation"}
            </span>
            {props.detail === "" ? null : (
              <span className="min-w-0 truncate text-muted-foreground/70">· {props.detail}</span>
            )}
            {props.elapsed === "" ? null : (
              <span
                className="ml-auto shrink-0 font-mono text-success text-xs tabular-nums"
                data-testid="live-row-elapsed"
              >
                {props.elapsed}
              </span>
            )}
          </div>
          <p className="text-[13px] text-muted-foreground" data-testid="live-row-sentence">
            {props.sentence}
          </p>
          <div className="mt-0.5 flex items-center gap-2">
            <Button
              size="xs"
              variant="outline"
              data-testid="view-conversation"
              onClick={props.onOpenConversation}
            >
              View conversation
            </Button>
            {props.onInterrupt === null ? null : (
              <Button
                size="xs"
                variant="outline"
                data-testid="live-row-interrupt"
                onClick={props.onInterrupt}
              >
                <SquareIcon className="size-3" />
                Interrupt
              </Button>
            )}
          </div>
        </div>
      </div>
    </li>
  );
}

export interface ActivityFeedProps {
  readonly rows: readonly ActivityRow[];
  /** The live row, rendered in its own place in time. Null with no conversation. */
  readonly live: React.ReactElement | null;
  /** Where the live row sits: the epoch millisecond it belongs to. */
  readonly liveAt: number;
  /** Null for a reader who may not write. */
  readonly onReply: ((body: string) => void) | null;
  readonly posting: boolean;
}

export function ActivityFeed(props: ActivityFeedProps): React.ReactElement {
  const groups = React.useMemo(() => foldActivity(props.rows), [props.rows]);
  const before: ActivityGroup[] = [];
  const after: ActivityGroup[] = [];
  for (const group of groups) {
    const at = group.kind === "row" ? group.row.at : (group.rows.at(-1)?.at ?? 0);
    (at <= props.liveAt ? before : after).push(group);
  }

  const draw = (group: ActivityGroup) =>
    group.kind === "fold" ? (
      <FoldRow key={group.key} rows={group.rows} />
    ) : group.row.kind === "comment" ? (
      <CommentCard
        key={group.key}
        row={group.row}
        onReply={props.onReply}
        posting={props.posting}
      />
    ) : (
      <EventRow key={group.key} row={group.row} />
    );

  return (
    <section data-testid="issue-activity">
      <h2 className="mb-1.5 font-semibold text-[15px]">Activity</h2>
      {groups.length === 0 && props.live === null ? (
        <p className="text-muted-foreground text-sm">Nothing has happened on this issue yet.</p>
      ) : (
        <ol className="flex flex-col">
          {before.map(draw)}
          {props.live}
          {after.map(draw)}
        </ol>
      )}
    </section>
  );
}
