import React from "react";

import type { BoardStats, ScopedWorkStats } from "../lib/model.ts";

export function StatsRow({
  stats,
  truncated,
  loadedCount,
  loading,
  totals = null,
}: {
  stats: BoardStats;
  truncated: boolean;
  loadedCount: number;
  loading: boolean;
  totals?: ScopedWorkStats | null;
}): React.ReactElement {
  const counters: readonly { key: string; value: number; label: string }[] = totals === null ? [
    { key: "running", value: stats.running, label: "observed running" },
    { key: "ready", value: stats.ready, label: "dispatchable items" },
    { key: "waiting", value: stats.waiting, label: "other open items" },
    { key: "blocked", value: stats.blocked, label: "blocked items" },
    { key: "completed", value: stats.completed, label: "terminal items" },
  ] : [
    { key: "running", value: totals.running, label: "observed running" },
    { key: "ready", value: totals.ready, label: "items in dispatchable lanes" },
    { key: "waiting", value: totals.waiting, label: "other open items" },
    { key: "completed", value: totals.completed, label: "terminal inventory" },
  ];
  return (
    <div
      data-testid="work-stats"
      className="flex flex-wrap items-center gap-x-5 gap-y-1 px-5 pb-3 text-muted-foreground text-xs"
    >
      <span>{loading ? "Refreshing…" : totals === null ? "Loaded results" : "Project scope · all filters"}</span>
      {counters.map((counter) => (
        <span key={counter.key} data-testid={`stat-${counter.key}`}>
          <b className="mr-1 font-semibold text-foreground tabular-nums">{counter.value}</b>
          {counter.label}
        </span>
      ))}
      <span className="flex-1" />
      <span className="font-mono text-[11px] tabular-nums" data-testid="stat-loaded">
        {stats.total} shown / {loadedCount} loaded items{totals === null ? "" : ` / ${totals.total} matching`}{truncated ? " (matching results remain)" : ""}
      </span>
      <span className="w-full" data-testid="stat-coverage">
        {totals === null ? null : <span className="mr-4" title={totals.asOf}>
          {totals.total} scoped items · Worker count observed {new Date(totals.asOf).toLocaleTimeString()}
          {truncated ? " · Partial result coverage; load more matching work" : " · All matching results loaded"}
        </span>}
        <span className="mr-2">Shown item observations:</span>
        {(["worker", "change"] as const).map((kind) => {
          const coverage = stats.observations[kind];
          return <span key={kind} className="mr-4">
            {kind === "worker" ? "Workers" : "Changes"}: {coverage.known} known · {coverage.unchecked} unchecked
            {" · "}{coverage.unavailable} unavailable · {coverage.partial} partial
          </span>;
        })}
      </span>
    </div>
  );
}
