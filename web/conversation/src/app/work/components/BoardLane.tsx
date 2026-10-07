import { ChevronDownIcon, ChevronRightIcon, PlusIcon } from "lucide-react";
import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import { cn } from "../../../lib/utils.ts";
import { isBlocked, isLive, type Lane, type WorkItemView } from "../lib/model.ts";
import { IssueCard } from "./IssueCard.tsx";

export interface BoardLaneProps {
  readonly lane: Lane;
  readonly collapsed: boolean;
  readonly onToggleCollapsed: () => void;
  readonly items: readonly WorkItemView[];
  readonly total?: number;
  readonly showProject: boolean;
  readonly now?: number;
  readonly onOpen: (item: WorkItemView) => void;
  readonly movesFor: (item: WorkItemView) => readonly string[];
  readonly onMove: (item: WorkItemView, toState: string) => void;
  readonly movingIds: ReadonlySet<string>;
  readonly draggingId: string | null;
  readonly onDragStart: (item: WorkItemView) => void;
  readonly onDragEnd: () => void;
  /** Null when the dragged card may not land here. */
  readonly onDrop: ((lane: Lane) => void) | null;
  readonly emptyLabel?: string;
  /** Opens the New issue dialog on this lane. Null when this lane takes no new issues. */
  readonly onCreate?: (() => void) | null;
}

export function BoardLane({
  lane,
  collapsed,
  onToggleCollapsed,
  items,
  total,
  showProject,
  now,
  onOpen,
  movesFor,
  onMove,
  movingIds,
  draggingId,
  onDragStart,
  onDragEnd,
  onDrop,
  emptyLabel,
  onCreate = null,
}: BoardLaneProps): React.ReactElement {
  const [over, setOver] = React.useState(false);
  const live = lane.terminal ? 0 : items.filter((item) => isLive(item)).length;
  const blocked = lane.name.toLowerCase().includes("block") || items.some((item) => isBlocked(item));

  return (
    <section
      aria-label={lane.name}
      data-testid="board-lane"
      data-lane={lane.name}
      data-terminal={lane.terminal ? "true" : undefined}
      onDragOver={(event) => {
        if (onDrop === null) return;
        event.preventDefault();
        if (event.dataTransfer !== null && event.dataTransfer !== undefined) {
          event.dataTransfer.dropEffect = "move";
        }
        setOver(true);
      }}
      onDragLeave={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget as Node | null)) setOver(false);
      }}
      onDrop={(event) => {
        setOver(false);
        if (onDrop === null) return;
        event.preventDefault();
        onDrop(lane);
      }}
      className={cn(
        "flex max-h-full shrink-0 flex-col rounded-xl border border-border bg-muted",
        collapsed ? "w-11" : "w-[300px]",
        over && "outline-2 outline-primary/60 outline-dashed",
      )}
    >
      <h2 className={cn(
        "flex items-center gap-2 pt-2.5 pb-2 font-semibold text-[13px]",
        collapsed ? "flex-col px-1" : "px-3",
      )}>
        <Button
          variant="ghost"
          size="icon-xs"
          data-testid={`lane-collapse-${lane.name}`}
          aria-expanded={!collapsed}
          aria-label={`${collapsed ? "Expand" : "Collapse"} ${lane.name}`}
          onClick={onToggleCollapsed}
          className="text-muted-foreground"
        >
          {collapsed ? <ChevronRightIcon /> : <ChevronDownIcon />}
        </Button>
        {collapsed ? null : <span className={cn(lane.terminal && "text-muted-foreground")}>{lane.name}</span>}
        <span
          data-testid={`lane-count-${lane.name}`}
          title={total === undefined ? "Loaded items" : `${items.length} shown / ${total} in project scope with server filters`}
          className={cn(
            "inline-grid h-5 min-w-5 place-items-center rounded-md px-1.5 font-normal text-[11px] tabular-nums",
            blocked && !lane.terminal
              ? "bg-error-surface text-error-foreground"
              : "bg-accent text-muted-foreground",
          )}
        >
          {total ?? items.length}
        </span>
        {collapsed ? <span className="mt-1 [writing-mode:vertical-rl]">{lane.name}</span> : null}
        {collapsed || live === 0 ? null : (
          <span className="font-normal text-[11px] text-muted-foreground">{live} live</span>
        )}
        {collapsed || onCreate === null ? null : (
          <Button
            variant="ghost"
            size="icon-xs"
            data-testid={`lane-new-issue-${lane.name}`}
            aria-label={`New issue in ${lane.name}`}
            title={`New issue in ${lane.name}`}
            onClick={onCreate}
            className="ml-auto text-muted-foreground"
          >
            <PlusIcon />
          </Button>
        )}
      </h2>
      {collapsed ? null : <div
        data-testid={`lane-body-${lane.name}`}
        className="flex min-h-16 flex-col gap-2 overflow-y-auto px-2 pb-2"
      >
        {items.length === 0 ? (
          <p className="px-3 py-6 text-center text-muted-foreground/70 text-xs">
            {emptyLabel ?? `Nothing is in ${lane.name.toLowerCase()}.`}
          </p>
        ) : (
          items.map((item) => (
            <IssueCard
              key={item.id}
              item={item}
              terminal={lane.terminal}
              showProject={showProject}
              now={now}
              onOpen={onOpen}
              moves={movesFor(item)}
              onMove={onMove}
              moving={movingIds.has(item.id)}
              dragging={draggingId === item.id}
              onDragStart={onDragStart}
              onDragEnd={onDragEnd}
            />
          ))
        )}
      </div>}
    </section>
  );
}
