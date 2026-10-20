import React from "react";
import { Link } from "@tanstack/react-router";
import type { FleetRunner, UpdatesReport } from "../../contracts/account.ts";
import { Button } from "../../components/ui/button.tsx";
import { cn } from "../../lib/utils.ts";
import { runnerUpdateStatusLabel } from "./format.ts";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { billingLabel, ProviderFreshness, UsageWindowCell, usageWindowKinds, usageWindowLabel } from "./ProviderUsage.tsx";
import { fleetHosts } from "./capacity.ts";

const SLOT_TONES = {
  running: "border-primary bg-primary",
  free: "border-border bg-background",
  unavailable: "border-dashed border-warning bg-warning/10",
};

const VERSION_TONES = {
  current: "text-success-foreground",
  patch: "text-warning-foreground",
  minor: "text-error-foreground",
};

function minorLine(version: string): string | null {
  return /^v?(\d+)\.(\d+)\./.exec(version)?.slice(1, 3).join(".") ?? null;
}

function versionLag(version: string, hub: string, behind: boolean): keyof typeof VERSION_TONES {
  if (!behind) return "current";
  const runner = minorLine(version);
  return runner !== null && runner === minorLine(hub) ? "patch" : "minor";
}

function stateTone(state: string): string {
  if (state === "Online") return "bg-success";
  if (state === "Offline" || state === "Asleep" || state === "Outside working hours" || state === "Paused" || state === "Status unavailable") return "bg-muted-foreground/50";
  return "bg-warning";
}

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

export function CapacityPanel({ runners, appUpdates, onOpen, showProviders = true, now }: {
  readonly runners: readonly FleetRunner[];
  readonly appUpdates?: UpdatesReport | null;
  readonly now?: number;
  readonly showProviders?: boolean;
  readonly onOpen?: (runner: FleetRunner) => void;
}): React.ReactElement {
  const hosts = fleetHosts(runners);
  const kinds = usageWindowKinds(hosts.flatMap((host) => host.runners.flatMap((runner) => runner.provider_capacity)));
  const updates = new Map(appUpdates?.runners.map((runner) => [runner.runner_id, runner]));
  const used = hosts.reduce((count, host) => count + host.used, 0);
  const total = hosts.reduce((count, host) => count + host.capacity, 0);
  const free = hosts.reduce((count, host) => count + host.free, 0);
  const unavailable = hosts.reduce((count, host) => count + Math.max(0, host.capacity - host.used - host.free), 0);
  return (
    <section aria-label="Capacity right now" className="min-w-0 space-y-3">
      <h2 className="text-sm font-medium">Capacity right now</h2>
      {hosts.length === 0 ? <p className="text-sm text-muted-foreground">No runners enrolled.</p> : <>
        <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
          <p className="text-base font-medium tabular-nums">{used} of {total} slots occupied</p>
          <p className="text-xs text-muted-foreground">{free} free</p>
          {unavailable > 0 ? <p className="text-xs text-warning-foreground">{unavailable} slots can't take work</p> : null}
          {appUpdates && appUpdates.behind_count > 0 ? <p className="text-xs text-warning-foreground">{appUpdates.behind_count} {appUpdates.behind_count === 1 ? "runner" : "runners"} behind the Hub</p> : null}
        </div>
        <p className="text-xs text-muted-foreground">Occupied slots include execution, finalization and merging across projects. Work board live counts follow its filters. New work also depends on project access and provider availability.{appUpdates ? " Versions are what each runner’s last heartbeat reported." : ""}</p>
        <Table className="w-auto text-xs">
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>Runner</TableHead>
              {appUpdates ? <TableHead>Version</TableHead> : null}
              <TableHead>Slots</TableHead>
              {showProviders ? <>
                <TableHead>Provider</TableHead>
                {kinds.map((kind) => <TableHead key={kind}>{usageWindowLabel(kind)}</TableHead>)}
                <TableHead className="sr-only">Freshness</TableHead>
              </> : null}
            </TableRow>
          </TableHeader>
          {hosts.map((host) => {
            const providers = showProviders ? host.runners.flatMap((runner) => runner.provider_capacity.map((capacity) => ({ runner, capacity }))) : [];
            const span = Math.max(1, providers.length);
            const hostCells = <>
              <TableCell rowSpan={span} className="align-top">
                <div className="space-y-1">
                  {host.runners.map((runner) => {
                    const state = runnerState(runner);
                    return <div key={runner.id} className="flex min-w-0 items-center gap-2">
                      <Tooltip>
                        <TooltipTrigger render={<span tabIndex={0} aria-label={state} className={cn("size-2 shrink-0 rounded-full", stateTone(state))} />} />
                        <TooltipPopup side="top">{state}</TooltipPopup>
                      </Tooltip>
                      <span className="sr-only">{state}</span>
                      {onOpen ? <Button variant="link" size="compact" onClick={() => onOpen(runner)} aria-label={"Open " + (runner.display_name || runner.hostname)}>{runner.display_name || runner.hostname}</Button> : <Link to="/settings/runners" className="min-w-0 text-sm font-medium hover:underline">{runner.display_name || runner.hostname}</Link>}
                    </div>;
                  })}
                </div>
              </TableCell>
              {appUpdates ? <TableCell rowSpan={span} className="align-top">
                <div className="space-y-1 leading-5">
                  {host.runners.map((runner) => {
                    const update = updates.get(runner.id);
                    if (!update?.version) return <div key={runner.id} className="text-muted-foreground">Not reported</div>;
                    const lag = versionLag(update.version, appUpdates.current, update.behind);
                    const label = update.behind ? `Behind the Hub ${appUpdates.current}${runner.update ? ` · ${runnerUpdateStatusLabel(runner.update.status)}` : ""}` : "Same as the Hub";
                    return <div key={runner.id}><Tooltip>
                      <TooltipTrigger render={<span tabIndex={0} aria-label={`${update.version} · ${label}`} data-version-lag={lag} className={cn("font-mono", VERSION_TONES[lag])} />}>{update.version}</TooltipTrigger>
                      <TooltipPopup side="top">{label}</TooltipPopup>
                    </Tooltip></div>;
                  })}
                </div>
              </TableCell> : null}
              <TableCell rowSpan={span} className="align-top">
                <div className="flex w-39 flex-wrap gap-1" aria-label={`${host.used} of ${host.capacity} slots occupied on ${host.runner.display_name || host.runner.hostname} · ${host.free} free · ${Math.max(0, host.capacity - host.used - host.free)} can't take work`}>
                  {Array.from({ length: Math.min(64, host.capacity) }, (_, index) => {
                    const lease = host.leases[index];
                    const state = index < host.used ? "running" : index < host.used + host.free ? "free" : "unavailable";
                    return <span key={index} data-slot-state={state} title={lease ? "#" + lease.work_item_id + " " + lease.title : state === "running" ? "Occupied slot" : state === "free" ? "Free slot" : "Can't take work"} className={cn("size-4 shrink-0 rounded-sm border", SLOT_TONES[state])} />;
                  })}
                  {host.capacity > 64 ? <span className="text-muted-foreground">+{host.capacity - 64}</span> : null}
                </div>
              </TableCell>
            </>;
            const empty = showProviders ? <TableCell colSpan={kinds.length + 2} /> : null;
            return <TableBody key={host.runner.machine_id} data-testid="runner-capacity" className="border-b border-border last:border-0 [&_tr]:border-0">
              {providers.length === 0 ? <TableRow className="hover:bg-transparent">{hostCells}{empty}</TableRow> : providers.map(({ runner, capacity }, index) => <TableRow key={runner.id + capacity.backend} className="hover:bg-transparent" aria-label={`${runner.display_name || runner.hostname} ${capacity.backend} provider capacity`}>
                {index === 0 ? hostCells : null}
                <TableCell>
                  <Tooltip>
                    <TooltipTrigger render={<span tabIndex={0} aria-label={`${capacity.backend} · ${billingLabel(capacity)}`} className="font-medium" />}>{host.runners.length > 1 ? `${runner.display_name || runner.hostname} · ` : ""}{capacity.backend}</TooltipTrigger>
                    <TooltipPopup side="top">{billingLabel(capacity)}</TooltipPopup>
                  </Tooltip>
                </TableCell>
                {capacity.billing === "api_key"
                  ? <TableCell colSpan={kinds.length + 1}><Link to="/usage" className="text-muted-foreground hover:underline">View runner-reported API spend in Usage</Link></TableCell>
                  : <>
                    {kinds.map((kind) => <TableCell key={kind}><UsageWindowCell capacity={capacity} kind={kind} now={now} /></TableCell>)}
                    <TableCell className="w-6"><ProviderFreshness capacity={capacity} now={now} /></TableCell>
                  </>}
              </TableRow>)}
            </TableBody>;
          })}
        </Table>
        <div className="flex flex-wrap gap-x-5 gap-y-2 text-xs text-muted-foreground" aria-label="Slot legend">
          {([["running", "Occupied"], ["free", "Free"], ["unavailable", "Can't take work"]] as const).map(([state, label]) => <span key={state} className="flex items-center gap-2"><span aria-hidden="true" className={cn("size-3 rounded-sm border", SLOT_TONES[state])} />{label}</span>)}
        </div>
      </>}
    </section>
  );
}
