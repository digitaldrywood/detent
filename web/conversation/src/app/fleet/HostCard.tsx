import React from "react";

import { Button } from "../../components/ui/button.tsx";
import type { FleetRunner } from "../../contracts/account.ts";
import { RunnerProblems } from "./RunnerProblems.tsx";
import { cn } from "../../lib/utils.ts";
import { RunnerStatusDot } from "../../components/RunnerStatusDot.tsx";
import type { RunnerProject } from "./RunnerDetailSheet.tsx";

export function HostCard({
  runner,
  projects = [],
  onOpen,
  onRemove,
}: {
  readonly runner: FleetRunner;
  readonly projects?: readonly RunnerProject[];
  readonly onOpen: () => void;
  readonly onRemove?: () => void;
}): React.ReactElement {
  const health = runner.health === "asleep" ? "Asleep, wakes on new work"
    : runner.health === "healthy" || runner.health === "online" ? "Healthy"
    : runner.health === "needs_attention" ? "Needs attention"
    : runner.health === "outside_hours" ? "Outside hours"
    : runner.health;
  const updateLabels: Record<string, string> = {
    requested: "Update requested",
    draining: "Waiting for active work",
    refused: "Update failed",
    uncertain: "Update needs attention",
    applied: "Update installed",
    restart_requested: "Restart requested",
    running: "Updated",
    drifted: "Version changed since update",
    unavailable: "Update unavailable",
  };
  const updateNeedsHuman = runner.update?.status === "refused" || runner.update?.status === "uncertain";
  const legacyReinstall = !!runner.claim_refusal_reason && (runner.update?.status === "unavailable" || /^v?0\.117\.(?:[0-9]|[1-4][0-9]|50)$/.test(runner.version ?? ""));
  const updateStatus = runner.update?.desired ? updateLabels[runner.update.status] ?? "Update pending" : undefined;
  return (
    <article
      id={`runner-${runner.id}`}
      tabIndex={-1}
      data-testid="host-card"
      className="grid min-w-0 gap-4 px-4 py-4 focus-visible:outline-2 focus-visible:outline-ring @[48rem]/runner-list:grid-cols-[minmax(0,1.2fr)_6rem_minmax(0,1fr)_7rem_minmax(0,1fr)_11rem] @[48rem]/runner-list:items-start"
    >
      <div className="flex min-w-0 items-start gap-2.5">
        <RunnerStatusDot runner={runner} className="mt-1.5" />
        <div className="min-w-0 space-y-1">
          <h3 className="break-words text-sm font-medium">{runner.display_name}</h3>
          {updateNeedsHuman ? <p className="text-xs text-warning-foreground">Needs human: reinstall the signed release with <code className="break-all">curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh</code>.</p> : legacyReinstall ? <p className="text-xs text-warning-foreground">One-time manual reinstall required to enable heartbeat updates: <code className="break-all">curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh</code>.</p> : null}
          {updateStatus ? <p className="text-xs text-muted-foreground">{updateStatus} · {runner.update?.desired?.version}</p> : null}
        </div>
      </div>
      <p className="text-xs text-muted-foreground">{runner.sprite ? "Fly Sprite" : "Own machine"}</p>
      <div className={cn("space-y-1 text-xs text-muted-foreground", runner.health === "needs_attention" && "text-warning-foreground")}>
        <p>{runner.claim_refusal_reason ? "Can’t take work" : health}</p>
        <p>{runner.state} · Limit {runner.capacity_limit}</p>
      </div>
      <div className="min-w-0 space-y-1 text-xs">
        <p className="text-muted-foreground @[48rem]/runner-list:sr-only">Running</p>
        <p className="text-muted-foreground tabular-nums">{runner.leases.length} of {runner.host_capacity} slots in use</p>
      </div>
      <p className="break-words text-xs text-muted-foreground">{runner.routing?.project_ids.map((id) => projects.find((project) => project.id === id)?.name ?? "Unavailable project").join(", ") || "No projects reported"}</p>
      <div className="flex flex-wrap gap-2">
        <Button size="xs" variant="outline" aria-label={`Manage ${runner.display_name}`} onClick={onOpen}>Manage</Button>
        {onRemove ? <Button size="xs" variant="outline" aria-label={`Remove runner ${runner.display_name}`} onClick={onRemove}>Remove runner</Button> : null}
      </div>
      {runner.problems?.length ? <div className="col-span-full min-w-0"><RunnerProblems runner={runner} alert needsHuman /></div> : null}
    </article>
  );
}
