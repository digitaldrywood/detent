import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import { cn } from "../../../lib/utils.ts";
import type { BoardStats, ScopedWorkStats } from "../lib/model.ts";

export function StatsRow({
  stats,
  hasMore,
  loadedCount,
  loading,
  onLoadMore,
  totals = null,
}: {
  stats: BoardStats;
  hasMore: boolean;
  loadedCount: number;
  loading: boolean;
  onLoadMore: () => void;
  totals?: ScopedWorkStats | null;
}): React.ReactElement {
  const counters = [
    { key: "running", value: totals?.running ?? stats.running, label: "running" },
    { key: "ready", value: totals?.ready ?? stats.ready, label: "ready" },
    { key: "waiting", value: totals?.waiting ?? stats.waiting + stats.blocked, label: "open" },
    { key: "completed", value: totals?.completed ?? stats.completed, label: "closed inventory" },
  ];
  const remaining = totals === null ? null : Math.max(0, totals.total - loadedCount);
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
                ? `${totals === null ? "Loaded items" : "All items"} in terminal states in the current filter scope, including cancelled and custom terminal states. Inventory, not shipping throughput. Imported terminal history: ${stats.importedClosed} of ${stats.completed} loaded closed items.`
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
      {hasMore ? (
        <Button size="xs" variant="outline" className="shrink-0 px-1.5 text-[10px] sm:px-2 sm:text-xs" disabled={loading} onClick={onLoadMore}>
          {totals === null ? "Load more" : `Load ${remaining} more · ${loadedCount} of ${totals.total}`}
        </Button>
      ) : null}
    </div>
  );
}
