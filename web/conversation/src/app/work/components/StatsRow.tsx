import { ChevronDownIcon } from "lucide-react";
import React from "react";

import { InlineButton } from "../../../components/ui/button.tsx";
import { Menu, MenuGroup, MenuGroupLabel, MenuPopup, MenuRadioGroup, MenuRadioItem, MenuTrigger } from "../../../components/ui/menu.tsx";
import { cn } from "../../../lib/utils.ts";
import { COMPLETED_WINDOWS, type CompletedWindow } from "../lib/viewState.ts";
import type { BoardStats, ScopedWorkStats } from "../lib/model.ts";

export function StatsRow({
  stats,
  completedWindow = "48h",
  onCompletedWindowChange,
  loading,
  totals = null,
}: {
  stats: BoardStats;
  completedWindow?: CompletedWindow;
  onCompletedWindowChange: (window: CompletedWindow) => void;
  loading: boolean;
  totals?: ScopedWorkStats | null;
}): React.ReactElement {
  const counters = [
    { key: "running", value: totals?.running ?? stats.running, label: "running", title: "Items in non-terminal lanes with a live worker." },
    { key: "waiting", value: totals?.waiting ?? stats.waiting, label: "waiting", title: "Items in dispatchable lanes with no live worker. Dependencies, human ownership or capacity may hold them." },
    { key: "need-attention", value: totals?.needAttention ?? stats.needAttention, label: "need attention", title: "Items in Triage, Blocked or Human Review." },
    { key: "backlog", value: totals?.backlog ?? stats.backlog, label: "backlog", title: "Items in Backlog. They are not dispatched until promoted." },
  ];
  return (
    <div
      data-testid="work-stats"
      aria-busy={loading}
      className="flex shrink-0 items-center justify-between gap-2 overflow-x-auto px-3 pb-1 text-muted-foreground text-[10px] sm:px-5 sm:text-xs"
    >
      <div className={cn("flex shrink-0 items-center gap-1.5 whitespace-nowrap sm:gap-2", loading && "opacity-50")}>
        {counters.map((counter, index) => (
          <React.Fragment key={counter.key}>
            {index === 0 ? null : <span aria-hidden>·</span>}
            <span
              data-testid={`stat-${counter.key}`}
              title={counter.title}
            >
              <b className="font-semibold text-foreground tabular-nums">{counter.value}</b>{" "}
              {counter.label}
            </span>
          </React.Fragment>
        ))}
        <span aria-hidden>·</span>
        <Menu>
          <MenuTrigger
            render={<InlineButton tone="muted" className="min-h-6" />}
            data-testid="stat-completed"
            title={`${completedWindow === "all" ? "All items in terminal states" : `Items that entered terminal states in the last ${completedWindow}`} in the current filter scope, including cancelled and custom terminal states. Choose the completed window.`}
          >
            <span>
              <b className="font-semibold text-foreground tabular-nums">{totals?.completed ?? (completedWindow === "all" ? stats.completed : "—")}</b>{" "}
              completed · {completedWindow === "all" ? "All time" : completedWindow}
            </span>
            <ChevronDownIcon aria-hidden className="size-3" />
          </MenuTrigger>
          <MenuPopup align="start" className="w-44">
            <MenuGroup>
              <MenuGroupLabel>Completed window</MenuGroupLabel>
              <MenuRadioGroup value={completedWindow} onValueChange={(value) => onCompletedWindowChange(value as CompletedWindow)}>
                {COMPLETED_WINDOWS.map((window) => (
                  <MenuRadioItem key={window} value={window} closeOnClick>
                    {window === "48h" ? "48 hours" : window === "7d" ? "7 days" : window === "14d" ? "14 days" : "All time"}
                  </MenuRadioItem>
                ))}
              </MenuRadioGroup>
            </MenuGroup>
          </MenuPopup>
        </Menu>
      </div>
    </div>
  );
}
