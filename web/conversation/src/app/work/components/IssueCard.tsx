import { GitPullRequestIcon } from "lucide-react";
import React from "react";

import { cn } from "../../../lib/utils.ts";
import { ProjectGlyph } from "../../components/ProjectGlyph.tsx";
import { ageLabel, elapsedLabel, issueNumber, projectHue } from "../lib/format.ts";
import { isBlocked, isLive, type WorkItemView } from "../lib/model.ts";
import { LaneMenu } from "./LaneMenu.tsx";

export const PILL_TONE = {
  mute: "border-border bg-muted text-muted-foreground",
  ok: "border-transparent bg-success/12 text-success-foreground",
  warn: "border-transparent bg-warning-surface text-warning-foreground",
  err: "border-transparent bg-error-surface text-error-foreground",
  info: "border-transparent bg-info/12 text-info-foreground",
} as const;

export type PillTone = keyof typeof PILL_TONE;

export function Pill({
  tone = "mute",
  className,
  children,
  ...rest
}: {
  tone?: PillTone;
  className?: string;
  children: React.ReactNode;
} & React.HTMLAttributes<HTMLSpanElement>): React.ReactElement {
  return (
    <span
      className={cn(
        "inline-flex h-5 shrink-0 items-center gap-1.5 whitespace-nowrap rounded-md border px-1.5 font-medium text-[11px] leading-none [&_svg]:size-3",
        PILL_TONE[tone],
        className,
      )}
      {...rest}
    >
      {children}
    </span>
  );
}

/** Urgent and High read as pressure; Normal and Low do not. */
export function priorityTone(priority: string | null): PillTone {
  switch (priority) {
    case "Urgent":
      return "err";
    case "High":
      return "warn";
    default:
      return "mute";
  }
}

/** The one sentence the card's status pill says about this issue. */
export function statusPill(item: WorkItemView): { tone: PillTone; label: string } | null {
  if (isLive(item)) return { tone: "ok", label: "Running" };
  if (isBlocked(item))
    return {
      tone: "err",
      label: item.blockedBy.length === 1 ? "Blocked · 1" : `Blocked · ${item.blockedBy.length}`,
    };
  if (item.attempt !== null && item.attempt.status === "failed")
    return { tone: "err", label: "Attempt failed" };
  if (item.attempt !== null && item.attempt.status === "interrupted")
    return { tone: "warn", label: "Interrupted" };
  return null;
}

export interface IssueCardProps {
  readonly item: WorkItemView;
  /** The project dot renders in the all-projects scope only (A.1, A.6). */
  readonly showProject: boolean;
  readonly now: number;
  readonly onOpen: (item: WorkItemView) => void;
  /** Lanes this card may move to, from the workflow's own transition list. */
  readonly moves: readonly string[];
  readonly onMove: (item: WorkItemView, toState: string) => void;
  /** True while an optimistic move is in flight. */
  readonly moving?: boolean;
  readonly dragging?: boolean;
  readonly onDragStart?: (item: WorkItemView) => void;
  readonly onDragEnd?: () => void;
}

export function IssueCard({
  item,
  showProject,
  now,
  onOpen,
  moves,
  onMove,
  moving = false,
  dragging = false,
  onDragStart,
  onDragEnd,
}: IssueCardProps): React.ReactElement {
  const live = isLive(item);
  const status = statusPill(item);
  const attempt = item.attempt;

  return (
    <article
      data-testid="issue-card"
      data-work-item={item.id}
      data-live={live ? "true" : undefined}
      draggable={moves.length > 0}
      onDragStart={(event) => {
        if (event.dataTransfer !== null && event.dataTransfer !== undefined) {
          event.dataTransfer.effectAllowed = "move";
          event.dataTransfer.setData("text/plain", item.id);
        }
        onDragStart?.(item);
      }}
      onDragEnd={onDragEnd}
      className={cn(
        "relative flex flex-col gap-1.5 rounded-[var(--radius)] border bg-card p-3 shadow-xs transition-colors",
        live ? "border-success/35 shadow-[0_0_0_1px_--theme(--color-success/8%)]" : "border-border",
        "hover:border-input",
        dragging && "opacity-50",
        moving && "animate-pulse",
      )}
    >
      <div className="flex items-center gap-1.5 text-muted-foreground text-xs">
        {showProject ? (
          <>
            <span
              aria-hidden
              data-testid="project-dot"
              className="size-2 shrink-0 rounded-full"
              style={{ backgroundColor: `oklch(0.72 0.15 ${projectHue(item.projectId)})` }}
            />
            <span className="min-w-0 truncate">{item.projectName}</span>
          </>
        ) : (
          <ProjectGlyph className="size-3.5" projectId={item.projectId} projectName={item.projectName} />
        )}
        <span className="ml-auto shrink-0 font-mono text-[11px] text-muted-foreground/70 tabular-nums">
          {issueNumber(item.identifier, item.number)}
        </span>
      </div>

      {/* The whole title is the link: a card is a destination, and a reader
          should not have to find the one word inside it that navigates. */}
      <button
        type="button"
        data-testid="issue-card-open"
        onClick={() => onOpen(item)}
        className="cursor-pointer rounded-sm text-left text-[13px] text-foreground leading-snug outline-none ring-ring focus-visible:ring-2"
      >
        {item.title}
      </button>

      {attempt !== null && attempt.running ? (
        <div
          data-testid="worker-strip"
          className="mt-0.5 flex items-center gap-2 overflow-hidden whitespace-nowrap rounded-lg border border-border bg-muted px-2.5 py-2 text-xs"
        >
          <span
            aria-hidden
            className="size-1.5 shrink-0 rounded-full bg-success motion-safe:animate-status-pulse"
          />
          <span className="text-foreground">{attempt.model ?? attempt.backend ?? "runner"}</span>
          <span className="min-w-0 truncate text-muted-foreground">
            {[attempt.backend, attempt.runner].filter((part) => part !== null).join(" · ")}
          </span>
          <span className="ml-auto shrink-0 text-muted-foreground tabular-nums">
            {elapsedLabel(attempt.startedAt, now)}
          </span>
        </div>
      ) : null}

      <div className="mt-0.5 flex items-center gap-1.5 text-[11px] text-muted-foreground">
        {status === null ? null : <Pill tone={status.tone}>{status.label}</Pill>}
        {item.change === null ? null : (
          <span
            data-testid="pr-chip"
            className="inline-flex items-center gap-1 text-muted-foreground"
            title={item.change.title}
          >
            <GitPullRequestIcon className="size-3" />
            {item.change.number === null ? "Change" : `PR #${item.change.number}`}
          </span>
        )}
        {attempt === null || attempt.attemptNumber === null || attempt.attemptNumber < 2 ? null : (
          <span>attempt {attempt.attemptNumber}</span>
        )}
        <span className="ml-auto shrink-0 tabular-nums" data-testid="issue-age">
          {ageLabel(item.updatedAt, now)}
        </span>
        {item.priority === null ? null : (
          <Pill tone={priorityTone(item.priority)}>{item.priority}</Pill>
        )}
        {/* Drag is a pointer gesture and nothing else, so every card carries
            the same move as a menu. A board a keyboard cannot reorder is a
            board half the readers cannot use (B.14). */}
        <LaneMenu item={item} lanes={moves} onMove={onMove} />
      </div>
    </article>
  );
}
