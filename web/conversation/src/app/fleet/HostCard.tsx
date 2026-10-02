import React from "react";

import { Button } from "../../components/ui/button.tsx";
import type { FleetRunner } from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";
import { formatRelativeTime } from "./format.ts";

function healthTone(health: string): string {
  if (health === "healthy") return "bg-success";
  if (health === "needs_attention") return "bg-warning";
  return "bg-muted-foreground";
}

export function HostCard({
  runner,
  now,
  onOpen,
}: {
  readonly runner: FleetRunner;
  readonly now?: number;
  readonly onOpen: () => void;
}): React.ReactElement {
  const health = runner.health === "asleep" ? "Asleep, wakes on new work"
    : runner.health === "healthy" ? "Healthy"
    : runner.health === "needs_attention" ? "Needs attention"
    : runner.health === "outside_hours" ? "Outside hours"
    : runner.health;
  return (
    <article
      id={`runner-${runner.id}`}
      tabIndex={-1}
      data-testid="host-card"
      className="grid min-w-0 gap-4 px-4 py-4 focus-visible:outline-2 focus-visible:outline-ring sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_7rem_auto] sm:items-start"
    >
      <div className="flex min-w-0 items-start gap-2.5">
        <span aria-hidden="true" className={cn("mt-1.5 size-2 shrink-0 rounded-full", healthTone(runner.health))} />
        <div className="min-w-0 space-y-1">
          <h3 className="break-words text-sm font-medium">{runner.display_name}</h3>
          <p className="break-words text-xs text-muted-foreground">{runner.hostname}, {runner.os} {runner.architecture}</p>
          <p className={cn("text-xs text-muted-foreground", runner.health === "needs_attention" && "text-warning")}><span>{health}</span> · {runner.state} · Limit {runner.capacity_limit}</p>
        </div>
      </div>
      <div className="min-w-0 space-y-1 text-xs">
        <p className="text-muted-foreground sm:sr-only">Running</p>
        <p className="text-muted-foreground tabular-nums">{runner.leases.length} of {runner.host_capacity} slots in use</p>
      </div>
      <div className="space-y-1 text-xs text-muted-foreground">
        <p className="sm:sr-only">Last check-in</p>
        <time dateTime={runner.last_heartbeat_at} title={runner.last_heartbeat_at}>{formatRelativeTime(runner.last_heartbeat_at, now)}</time>
      </div>
      <Button size="xs" variant="outline" className="w-fit" aria-label={`Manage ${runner.display_name}`} onClick={onOpen}>Manage</Button>
    </article>
  );
}
