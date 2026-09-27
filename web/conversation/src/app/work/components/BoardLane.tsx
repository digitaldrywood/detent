// One lane column (artifact screen 1's `.lane`).
//
// A terminal lane renders dimmed and collapsed, which is the artifact's own
// `.lane.dim` treatment for `Merging`: a state that closes work is evidence,
// not a queue, and it should not take a third of the board's width from the
// lanes that still need someone.
//
// The drop target is the lane body. It accepts a drop only when the dragged
// card's workflow allows this lane, so a card cannot be dropped into a lane
// the hub would refuse — the refusal is prevented rather than reported.
import { ChevronDownIcon, ChevronRightIcon } from "lucide-react";
import React from "react";

import { cn } from "../../../lib/utils.ts";
import type { Lane, WorkItemView } from "../lib/model.ts";
import { IssueCard } from "./IssueCard.tsx";

export interface BoardLaneProps {
  readonly lane: Lane;
  readonly items: readonly WorkItemView[];
  readonly showProject: boolean;
  readonly now: number;
  readonly onOpen: (item: WorkItemView) => void;
  readonly movesFor: (item: WorkItemView) => readonly string[];
  readonly onMove: (item: WorkItemView, toState: string) => void;
  readonly movingIds: ReadonlySet<string>;
  readonly draggingId: string | null;
  readonly onDragStart: (item: WorkItemView) => void;
  readonly onDragEnd: () => void;
  /** Null when the dragged card may not land here. */
  readonly onDrop: ((lane: Lane) => void) | null;
  readonly collapsed: boolean;
  readonly onToggleCollapsed: () => void;
  readonly emptyLabel?: string;
}

export function BoardLane({
  lane,
  items,
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
  collapsed,
  onToggleCollapsed,
  emptyLabel,
}: BoardLaneProps): React.ReactElement {
  const [over, setOver] = React.useState(false);
  const live = items.filter((item) => item.attempt?.running === true).length;
  const blocked = lane.name.toLowerCase().includes("block") || items.some((item) => item.blockedBy.length > 0);

  return (
    <section
      aria-label={lane.name}
      data-testid="board-lane"
      data-lane={lane.name}
      data-terminal={lane.terminal ? "true" : undefined}
      className={cn(
        "flex max-h-full shrink-0 flex-col rounded-xl border border-border bg-muted",
        collapsed ? "w-11" : "w-[300px]",
        lane.terminal && "opacity-70",
      )}
    >
      <h2 className={cn("flex items-center gap-2 px-3 pt-2.5 pb-2 font-semibold text-[13px]", collapsed && "flex-col px-0")}>
        <button
          type="button"
          data-testid={`lane-collapse-${lane.name}`}
          aria-expanded={!collapsed}
          aria-label={`${collapsed ? "Expand" : "Collapse"} ${lane.name}`}
          onClick={onToggleCollapsed}
          className="inline-flex cursor-pointer items-center gap-1.5 rounded-sm outline-none ring-ring focus-visible:ring-2"
        >
          {collapsed ? (
            <ChevronRightIcon className="size-3.5 text-muted-foreground" />
          ) : (
            <ChevronDownIcon className="size-3.5 text-muted-foreground" />
          )}
          <span className={cn(collapsed && "sr-only", lane.terminal && "text-muted-foreground")}>
            {lane.name}
          </span>
        </button>
        {collapsed ? (
          <span className="mt-1 [writing-mode:vertical-rl] text-muted-foreground text-xs">
            {lane.name}
          </span>
        ) : null}
        <span
          data-testid={`lane-count-${lane.name}`}
          className={cn(
            "inline-grid h-5 min-w-5 place-items-center rounded-md px-1.5 font-normal text-[11px] tabular-nums",
            blocked && !lane.terminal
              ? "bg-error-surface text-error-foreground"
              : "bg-accent text-muted-foreground",
          )}
        >
          {items.length}
        </span>
        {collapsed || live === 0 ? null : (
          <span className="font-normal text-[11px] text-muted-foreground">{live} live</span>
        )}
      </h2>
      {collapsed ? null : (
        <div
          data-testid={`lane-body-${lane.name}`}
          onDragOver={(event) => {
            if (onDrop === null) return;
            event.preventDefault();
            // `dataTransfer` is absent on a synthesised drag event, which is
            // what a test fires and what some assistive tooling produces.
            if (event.dataTransfer !== null && event.dataTransfer !== undefined) {
              event.dataTransfer.dropEffect = "move";
            }
            setOver(true);
          }}
          onDragLeave={() => setOver(false)}
          onDrop={(event) => {
            setOver(false);
            if (onDrop === null) return;
            event.preventDefault();
            onDrop(lane);
          }}
          className={cn(
            "flex min-h-16 flex-col gap-2 overflow-y-auto px-2 pb-2",
            over && "rounded-b-xl outline-2 outline-primary/60 outline-dashed",
          )}
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
        </div>
      )}
    </section>
  );
}
