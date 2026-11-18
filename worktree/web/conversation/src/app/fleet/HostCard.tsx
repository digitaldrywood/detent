import { MoreHorizontalIcon } from "lucide-react";
import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Menu, MenuTrigger, MenuPopup, MenuItem } from "../../components/ui/menu.tsx";
import type { FleetRunner } from "../../contracts/account.ts";
import { RunnerStatusDot } from "../../components/RunnerStatusDot.tsx";
import type { RunnerProject } from "./RunnerDetailPage.tsx";
import { runnerReason, runnerUpdateFailed } from "./runnerPresentation.ts";
import { RunnerCapacity } from "./RunnerCapacity.tsx";
import { formatRelativeTime } from "./format.ts";

export function HostCard({ runner, now, projects = [], onOpen, onRemove }: {
  readonly runner: FleetRunner;
  readonly now?: number;
  readonly projects?: readonly RunnerProject[];
  readonly onOpen: () => void;
  readonly onRemove?: () => void;
}): React.ReactElement {
  const reason = runnerReason(runner);
  const version = runner.has_heartbeat === false ? undefined : runner.version?.trim();
  const runnerProjects = runner.routing?.scope === "organization" ? projects
    : runner.routing?.project_ids.map((id) => projects.find((project) => project.id === id) ?? { id, name: "Unavailable project" }) ?? [];
  const projectNames = runner.routing?.scope === "organization" ? "All projects" : runner.routing?.project_ids.map((id) => projects.find((project) => project.id === id)?.name ?? "Unavailable project").join(", ") || "No projects reported";
  return <article id={`runner-${runner.id}`} data-testid="host-card" className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-3 px-4 py-3 @[48rem]/runner-list:grid-cols-[minmax(0,1.5fr)_minmax(0,1fr)_minmax(0,1.4fr)_minmax(0,1fr)_7rem] @[48rem]/runner-list:gap-4">
    <div className="flex min-w-0 items-start gap-2.5">
      <RunnerStatusDot runner={runner} className="mt-1.5" />
      <div className="min-w-0 space-y-1">
        <h3 className="truncate text-sm font-medium" title={runner.display_name}>{runner.display_name}</h3>
        {reason ? <p className="truncate text-xs text-muted-foreground" title={reason}>{reason}</p> : null}
      </div>
    </div>
    <div className="col-start-1 flex flex-wrap items-center gap-1.5 @[48rem]/runner-list:col-auto" title={`Last check-in ${formatRelativeTime(runner.last_heartbeat_at, now)}`}>
      <Badge variant="outline" size="sm">{version || "Version unknown"}</Badge>
      {runnerUpdateFailed(runner) ? <Badge variant="warning" size="sm">Update failed</Badge> : null}
    </div>
    <div className="col-start-1 min-w-0 text-xs @[48rem]/runner-list:col-auto">
      <RunnerCapacity runner={runner} />
    </div>
    <div className="col-start-1 min-w-0 space-y-1 break-words text-xs text-muted-foreground @[48rem]/runner-list:col-auto">
      {runnerProjects.map((project) => {
        const capacity = runner.capacity_configuration?.projects?.[project.id];
        const maximum = runner.routing?.project_capacity_limits?.[project.id];
        return <p key={project.id}>{project.name}: {capacity?.used ?? "Unknown"} in use · maximum {maximum ?? "inherited"}{capacity ? ` · effective ${capacity.effective ?? "unknown"}` : ""}</p>;
      })}
      {runnerProjects.length === 0 ? <p>{projectNames}</p> : null}
    </div>
    <div className="col-start-2 row-start-1 flex items-center justify-end gap-1 @[48rem]/runner-list:col-auto @[48rem]/runner-list:row-auto">
      <Button size="xs" variant="outline" aria-label={`Manage ${runner.display_name}`} onClick={onOpen}>Manage</Button>
      {onRemove ? <Menu>
        <MenuTrigger render={<Button size="icon-xs" variant="ghost" aria-label={`More actions for ${runner.display_name}`}><MoreHorizontalIcon aria-hidden="true" /></Button>} />
        <MenuPopup align="end"><MenuItem variant="destructive" onClick={onRemove}>Remove runner</MenuItem></MenuPopup>
      </Menu> : null}
    </div>
  </article>;
}
