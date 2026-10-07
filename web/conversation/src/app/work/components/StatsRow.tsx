import React from "react";

import { cn } from "../../../lib/utils.ts";
import type { CompletedWindow } from "../lib/viewState.ts";
import type { BoardStats, ScopedWorkStats } from "../lib/model.ts";

export function StatsRow({
  stats,
  completedWindow = "48h",
  loading,
  totals = null,
}: {
  stats: BoardStats;
  completedWindow?: CompletedWindow;
  loading: boolean;
  totals?: ScopedWorkStats | null;
}): React.ReactElement {
  const counters = [
    { key: "running", value: totals?.running ?? stats.running, label: "running" },
    { key: "queued", value: totals?.queued ?? stats.queued, label: "queued inventory" },
    { key: "open", value: totals?.open ?? stats.open, label: "open" },
    { key: "completed", value: totals?.completed ?? (completedWindow === "all" ? stats.completed : "—"), label: `completed · ${completedWindow}` },
  ];
  return (
    <div
      data-testid="work-stats"
      aria-busy={loading}
      className="flex shrink-0 items-center justify-between gap-2 overflow-x-auto px-3 pb-3 text-muted-foreground text-[10px] sm:px-5 sm:text-xs"
    >
      <div className={cn("flex shrink-0 items-center gap-1.5 whitespace-nowrap sm:gap-2", loading && "opacity-50")}>
        {counters.map((counter, index) => (
          <React.Fragment key={counter.key}>
            {index === 0 ? null : <span aria-hidden>·</span>}
            <span
              data-testid={`stat-${counter.key}`}
              title={counter.key === "completed"
                ? `${completedWindow === "all" ? "All items in terminal states" : `Items that entered terminal states in the last ${completedWindow}`} in the current filter scope, including cancelled and custom terminal states. Inventory, not shipping throughput. Imported terminal history: ${stats.importedClosed} of ${stats.completed} loaded closed items.`
                : counter.key === "queued"
                  ? "Items in Backlog or dispatchable lanes without a live worker in the current filter scope. Human ownership, dependencies and other dispatch conditions may hold them."
                  : undefined}
            >
              <b className="font-semibold text-foreground tabular-nums">{counter.value}</b>{" "}
              {counter.label}
              {counter.key === "completed" && stats.importedClosed > 0
                ? ` (${stats.importedClosed} imported history loaded)`
                : null}
            </span>
          </React.Fragment>
        ))}
      </div>
    </div>
  );
}
