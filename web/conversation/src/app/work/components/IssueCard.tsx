import { GitPullRequestIcon } from "lucide-react";
import React from "react";

import { cn } from "../../../lib/utils.ts";
import { ProjectGlyph } from "../../components/ProjectGlyph.tsx";
import { ageLabel, elapsedLabel, issueNumber, projectHue } from "../lib/format.ts";
import { isBlocked, isLive, type WorkItemView } from "../lib/model.ts";
import { useNow } from "../lib/useWork.ts";
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
export function priorityTone(priority: string | null, terminal = false): PillTone {
  if (terminal) return "mute";
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
export function statusPill(item: WorkItemView, terminal = item.terminal): { tone: PillTone; label: string } | null {
  if (terminal) return { tone: "mute", label: item.state };
  if (isLive(item, terminal)) return { tone: "ok", label: "Running" };
  if (isBlocked(item, terminal))
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
  readonly terminal?: boolean;
  /** The project dot renders in the all-projects scope only (A.1, A.6). */
  readonly showProject: boolean;
  readonly now?: number;
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

export function AgeText({ at, now }: { at: string | null | undefined; now?: number }): React.ReactElement {
  const current = useNow();
  return <>{ageLabel(at, now ?? current)}</>;
}

export function ElapsedText({ at, now }: { at: string | null | undefined; now?: number }): React.ReactElement {
  const current = useNow();
  return <>{elapsedLabel(at, now ?? current)}</>;
}

function sourceDescription(item: WorkItemView, now: number): string | null {
  return item.sourceProvider === null ? null : [
    `Imported from ${item.sourceProvider}${item.source === null ? "" : ` · ${item.source.externalId}`}`,
    ...[
      ["Original source created", item.source?.createdAt],
      ["Original source updated", item.source?.updatedAt],
      ["Source observed", item.source?.observedAt],
    ].map(([label, at]) => {
      const sourceAge = ageLabel(at, now);
      return `${label}: ${sourceAge === "" ? "unavailable" : `${sourceAge} ago (${at})`}`;
    }),
  ].join("\n");
}

function SourceBadge({ item, terminal, now }: { item: WorkItemView; terminal: boolean; now?: number }): React.ReactElement {
  const current = useNow();
  return <span className="shrink-0 whitespace-nowrap text-[11px]" title={sourceDescription(item, now ?? current) ?? undefined}>
    {terminal ? "Source history" : "Imported"}
  </span>;
}

function IssueAge({ item, now }: { item: WorkItemView; now?: number }): React.ReactElement {
  const current = useNow();
  const clock = now ?? current;
  const age = ageLabel(item.updatedAt, clock);
  const sourceContext = sourceDescription(item, clock);
  const updateContext = `${item.sourceProvider === null ? "Updated" : "Native update"}: ${age === "" ? "unavailable" : `${age} ago (${item.updatedAt})`}`;
  return <span className="ml-auto shrink-0 tabular-nums" data-testid="issue-age"
    title={[updateContext, sourceContext].filter((part) => part !== null).join("\n")}>
    {age === "" ? "" : `${item.sourceProvider === null ? "Updated" : "Native update"} ${age}`}
  </span>;
}

export function IssueCard({
  item,
  terminal = item.terminal,
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
  const live = isLive(item, terminal);
  const status = statusPill(item, terminal);
  const attempt = item.attempt;

  return (
    <article
      data-testid="issue-card"
      data-work-item={item.id}
      data-live={live ? "true" : undefined}
      data-terminal={terminal ? "true" : undefined}
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
        terminal && "bg-muted shadow-none",
        "hover:border-input",
        dragging && "opacity-50",
        moving && !terminal && "animate-pulse",
      )}
    >
      <div className="flex items-center gap-1.5 text-muted-foreground text-xs">
        {showProject ? (
          <>
            <span
              aria-hidden
              data-testid="project-dot"
              className={cn("size-2 shrink-0 rounded-full", terminal && "bg-muted-foreground")}
              style={terminal ? undefined : {
                backgroundColor: `oklch(0.72 0.15 ${projectHue(item.projectId)})`,
              }}
            />
            <span className="min-w-0 truncate">{item.projectName}</span>
          </>
        ) : (
          <span className="inline-flex" title={item.projectName}>
            <ProjectGlyph
              className={cn("size-3.5", terminal && "text-muted-foreground grayscale")}
              projectId={item.projectId}
              projectName={item.projectName}
            />
            <span className="sr-only">{item.projectName}</span>
          </span>
        )}
        {item.sourceProvider === null ? null : <SourceBadge item={item} terminal={terminal} now={now} />}
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

      {(live && attempt !== null) || (item.reserveWorkerSpace && !terminal) ? (
        <div
          data-testid={live && attempt !== null ? "worker-strip" : undefined}
          aria-busy={item.observations?.worker === "unchecked"}
          className={cn("mt-0.5 flex h-9 items-center gap-2 overflow-hidden whitespace-nowrap rounded-lg px-2.5 text-xs",
            live && attempt !== null && "border border-border bg-muted")}
        >
          {live && attempt !== null ? (
            <>
              <span
                aria-hidden
                className="size-1.5 shrink-0 rounded-full bg-success motion-safe:animate-status-pulse"
              />
              <span className="text-foreground">{attempt.model ?? attempt.backend ?? "runner"}</span>
              <span className="min-w-0 truncate text-muted-foreground">
                {[attempt.backend, attempt.runner].filter((part) => part !== null).join(" · ")}
              </span>
              <span className="ml-auto shrink-0 text-muted-foreground tabular-nums">
                <ElapsedText at={attempt.startedAt} now={now} />
              </span>
            </>
          ) : null}
        </div>
      ) : null}

      <div className="mt-0.5 flex min-h-5 items-center gap-1.5 text-[11px] text-muted-foreground">
        {status === null ? null : (
          <Pill
            tone={status.tone}
            title={terminal && attempt !== null ? `Last attempt: ${attempt.status}` : undefined}
            aria-label={terminal && attempt !== null ? `${status.label}. Last attempt: ${attempt.status}` : undefined}
          >
            {status.label}
          </Pill>
        )}
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
        <IssueAge item={item} now={now} />
        {item.priority === null ? null : (
          <Pill tone={priorityTone(item.priority, terminal)} aria-label={`Priority: ${item.priority}`}>
            {item.priority}
          </Pill>
        )}
        {/* Drag is a pointer gesture and nothing else, so every card carries
            the same move as a menu. A board a keyboard cannot reorder is a
            board half the readers cannot use (B.14). */}
        <LaneMenu item={item} lanes={moves} onMove={onMove} />
      </div>
    </article>
  );
}
