import React from "react";

import { Button } from "../../components/ui/button.tsx";
import {
  Dialog, DialogHeader, DialogPanel, DialogPopup, DialogTitle, DialogTrigger,
} from "../../components/ui/dialog.tsx";
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
  settings,
}: {
  readonly runner: FleetRunner;
  readonly now?: number;
  readonly settings?: React.ReactNode;
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
          <p className={cn("text-xs text-muted-foreground", runner.health === "needs_attention" && "text-warning")}>{health}</p>
        </div>
      </div>
      <div className="min-w-0 space-y-1 text-xs">
        <p className="text-muted-foreground sm:sr-only">Running</p>
        <ul className="space-y-1">
          {runner.leases.map((lease) => (
            <li key={lease.lease_id} className="truncate" title={`#${lease.work_item_id} ${lease.title}`}>
              <span className="font-medium">#{lease.work_item_id}</span> {lease.title}
            </li>
          ))}
        </ul>
        <p className="text-muted-foreground tabular-nums">{runner.leases.length} of {runner.host_capacity} slots in use</p>
      </div>
      <div className="space-y-1 text-xs text-muted-foreground">
        <p className="sm:sr-only">Last check-in</p>
        <time dateTime={runner.last_heartbeat_at} title={runner.last_heartbeat_at}>{formatRelativeTime(runner.last_heartbeat_at, now)}</time>
      </div>
      <Dialog>
        <DialogTrigger render={<Button size="xs" variant="outline" className="w-fit" aria-label={`Manage ${runner.display_name}`}>Manage</Button>} />
        <DialogPopup className="sm:max-w-2xl">
          <DialogHeader><DialogTitle>{runner.display_name}</DialogTitle></DialogHeader>
          <DialogPanel className="space-y-4 text-xs">
            <p className="text-muted-foreground">{runner.state} · {runner.hostname} · {runner.os}/{runner.architecture}</p>
            <p>Runner limit: {runner.capacity_limit}{runner.reported_capacity !== runner.capacity_limit ? ` (reports ${runner.reported_capacity})` : ""}</p>
            {runner.isolation_tier === undefined ? null : (
              <p className="text-muted-foreground">
                Isolation setting: {runner.isolation_tier === "native-trusted" ? "Trusted only: full host access" : "Sandbox"}
                {runner.availability?.windows.length ? ` · ${runner.availability.windows.join(", ")} (${runner.availability.timezone})` : " · Always available"}
              </p>
            )}
            {(runner.home_project_ids?.length ?? 0) === 0 ? null : (
              <div data-testid="home-status" className="break-words text-muted-foreground">
                <p>Home projects: {runner.home_project_ids?.join(", ")}</p>
                {runner.home_status ? <p>{runner.home_status}</p> : null}
              </div>
            )}
            <ul className="space-y-2 text-muted-foreground">
              {runner.provider_capacity.map((capacity) => (
                <li key={`${capacity.provider}-${capacity.account_alias}`} className="break-words">
                  {capacity.provider} · {capacity.account_alias} · {capacity.availability} · {capacity.state} · {capacity.used}/{capacity.max_concurrent}
                </li>
              ))}
            </ul>
            {settings}
          </DialogPanel>
        </DialogPopup>
      </Dialog>
    </article>
  );
}
