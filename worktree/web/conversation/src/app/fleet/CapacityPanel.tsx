import React from "react";
import { Link } from "@tanstack/react-router";
import type { FleetRunner } from "../../contracts/account.ts";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { cn } from "../../lib/utils.ts";
import { fleetHosts } from "./capacity.ts";

const SLOT_TONES = {
  running: "border-primary bg-primary",
  free: "border-border bg-background",
  unavailable: "border-dashed border-warning bg-warning/10",
};

function runnerState(runner: FleetRunner): string {
  if (runner.connection_health === "offline" || runner.health === "offline") return "Offline";
  if (runner.update?.status === "draining") return "Finishing work before updating";
  if (runner.state === "draining") return "Finishing current work";
  if (runner.state === "disabled") return "Paused";
  if (runner.problems?.some((problem) => problem.code === "policy_mismatch")) return "Project policy needs review";
  if (runner.claim_refusal_reason) return "Cannot take new work";
  if (runner.update?.status === "applying") return "Updating";
  if (runner.update?.status === "uncertain" || runner.update?.status === "refused") return "Update needs attention";
  return ({ online: "Online", asleep: "Asleep", outside_hours: "Outside working hours", needs_attention: "Needs attention" } as Record<string, string>)[runner.health] ?? "Status unavailable";
}

export function CapacityPanel({ runners, onOpen }: {
  readonly runners: readonly FleetRunner[];
  readonly onOpen?: (runner: FleetRunner) => void;
}): React.ReactElement {
  const hosts = fleetHosts(runners);
  const used = hosts.reduce((count, host) => count + host.used, 0);
  const total = hosts.reduce((count, host) => count + host.capacity, 0);
  const free = hosts.reduce((count, host) => count + host.free, 0);
  const unavailable = hosts.reduce((count, host) => count + Math.max(0, host.capacity - host.used - host.free), 0);
  return (
    <section aria-label="Capacity right now" className="min-w-0 space-y-3">
      <h2 className="text-sm font-medium">Capacity right now</h2>
      {hosts.length === 0 ? <p className="text-sm text-muted-foreground">No runners enrolled.</p> : <>
        <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
          <p className="text-base font-medium tabular-nums">{used} of {total} slots running work</p>
          <p className="text-xs text-muted-foreground">{free} free</p>
          {unavailable > 0 ? <p className="text-xs text-warning-foreground">{unavailable} slots can't take work</p> : null}
        </div>
        <p className="text-xs text-muted-foreground">Reported capacity; new work also depends on project access and provider availability.</p>
        <div className="divide-y divide-border">
          {hosts.map((host) => (
            <div key={host.runner.machine_id} className="grid min-w-0 gap-2 py-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)] sm:items-center" data-testid="runner-capacity">
              <div className="min-w-0 space-y-1">
                {host.runners.map((runner) => <div key={runner.id} className="flex min-w-0 flex-wrap items-center gap-2">
                  {onOpen ? <Button variant="link" size="compact" onClick={() => onOpen(runner)} aria-label={"Open " + (runner.display_name || runner.hostname)}>{runner.display_name || runner.hostname}</Button> : <Link to="/settings/runners" className="min-w-0 break-all text-sm font-medium hover:underline">{runner.display_name || runner.hostname}</Link>}
                  <Badge variant="secondary">{runnerState(runner)}</Badge>
                </div>)}
                <p className="text-xs text-muted-foreground tabular-nums">{host.used} running · {host.free} free · {Math.max(0, host.capacity - host.used - host.free)} unavailable</p>
              </div>
              <div className="flex min-w-0 flex-wrap gap-1.5" aria-label={`${host.used} of ${host.capacity} slots running work on ${host.runner.display_name || host.runner.hostname}`}>
                {Array.from({ length: Math.min(64, host.capacity) }, (_, index) => {
                  const lease = host.leases[index];
                  const state = index < host.used ? "running" : index < host.used + host.free ? "free" : "unavailable";
                  return <span key={index} data-slot-state={state} title={lease ? "#" + lease.work_item_id + " " + lease.title : state === "running" ? "Running work" : state === "free" ? "Free slot" : "Can't take work"} className={cn("size-5 shrink-0 rounded-sm border", SLOT_TONES[state])} />;
                })}
                {host.capacity > 64 ? <span className="text-xs text-muted-foreground">+{host.capacity - 64} slots</span> : null}
              </div>
            </div>
          ))}
        </div>
        <div className="flex flex-wrap gap-x-5 gap-y-2 text-xs text-muted-foreground" aria-label="Slot legend">
          {([["running", "Running work"], ["free", "Free"], ["unavailable", "Can't take work"]] as const).map(([state, label]) => <span key={state} className="flex items-center gap-2"><span aria-hidden="true" className={cn("size-3 rounded-sm border", SLOT_TONES[state])} />{label}</span>)}
        </div>
      </>}
    </section>
  );
}
