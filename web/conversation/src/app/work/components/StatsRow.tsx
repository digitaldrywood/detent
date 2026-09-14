// The stats strip (artifact screen 1's `.stats`).
//
// Every counter is derived from the issues on screen, so the strip and the
// lanes can never disagree. The artifact's trailing mono line —
// `1 / 2 slots · 312 tps · $28.77 notional today` — is not rendered: slots,
// throughput and notional spend are fleet figures, and no endpoint on the work
// API serves any of them. What stands in its place is the one thing this
// client does know about its own reading: how much of the board it has, and
// how many of its issues it was able to ask about a running attempt and a
// change request — two questions that cost one request each, per issue, and so
// are asked of a bounded subset (`ENRICH_LIMIT` in `lib/useWork.ts`).
import React from "react";

import type { BoardStats } from "../lib/model.ts";

export function StatsRow({
  stats,
  truncated,
  enriched,
}: {
  stats: BoardStats;
  truncated: boolean;
  enriched: number;
}): React.ReactElement {
  const counters: readonly { key: string; value: number; label: string }[] = [
    { key: "running", value: stats.running, label: "running" },
    { key: "ready", value: stats.ready, label: "ready" },
    { key: "waiting", value: stats.waiting, label: "waiting" },
    { key: "blocked", value: stats.blocked, label: "blocked" },
    { key: "completed", value: stats.completed, label: "completed" },
  ];
  return (
    <div
      data-testid="work-stats"
      className="flex flex-wrap items-center gap-x-5 gap-y-1 px-5 pb-3 text-muted-foreground text-xs"
    >
      {counters.map((counter) => (
        <span key={counter.key} data-testid={`stat-${counter.key}`}>
          <b className="mr-1 font-semibold text-foreground tabular-nums">{counter.value}</b>
          {counter.label}
        </span>
      ))}
      <span className="flex-1" />
      <span className="font-mono text-[11px] tabular-nums" data-testid="stat-coverage">
        {stats.total} issues
        {truncated ? " (first page)" : ""} · {enriched} checked for a worker and a change
      </span>
    </div>
  );
}
