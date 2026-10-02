import { AlertTriangleIcon, BotIcon, ServerIcon } from "lucide-react";
import React from "react";

import { Button } from "../../components/ui/button.tsx";
import type { FleetResponse, FleetRunner, ProviderCapacity, RunnerRouting } from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useResource } from "../account/useResource.ts";
import { SettingsPageContainer, SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import {
  EnrollRunnerDialog,
  PendingEnrollments,
  type PendingEnrollment,
} from "./EnrollRunner.tsx";
import { HostCard } from "./HostCard.tsx";
import { RunnerDetailSheet, type RunnerProject } from "./RunnerDetailSheet.tsx";
import { PathValue } from "../account/controls.tsx";
import { RUNNER_UPGRADE_COMMAND } from "../lib/detentUpdates.ts";
import { formatLocalTime } from "./format.ts";

export interface ProviderRow {
  readonly provider: string;
  readonly accounts: number;
  readonly used: number;
  readonly max: number;
  readonly availability: readonly string[];
  readonly status: readonly string[];
  readonly models: readonly string[];
}

export function summarizeProviders(runners: readonly FleetRunner[]): readonly ProviderRow[] {
  const rows = new Map<
    string,
    {
      provider: string;
      accounts: Set<string>;
      used: number;
      max: number;
      availability: Set<string>;
      status: Set<string>;
      models: Set<string>;
    }
  >();
  const capacities: ProviderCapacity[] = runners.flatMap((runner) => [...runner.provider_capacity]);
  for (const capacity of capacities) {
    let row = rows.get(capacity.provider);
    if (row === undefined) {
      row = {
        provider: capacity.provider,
        accounts: new Set(),
        used: 0,
        max: 0,
        availability: new Set(),
        status: new Set(),
        models: new Set(),
      };
      rows.set(capacity.provider, row);
    }
    row.accounts.add(capacity.account_alias);
    row.used += capacity.used;
    row.max += capacity.max_concurrent;
    row.availability.add(capacity.availability);
    row.status.add(capacity.reset_at ? "Rate limited until " + formatLocalTime(capacity.reset_at)
      : [capacity.availability, capacity.state].filter(Boolean).map((value) => value.replaceAll("_", " ")).join(" · "));
    for (const model of capacity.models ?? []) row.models.add(model);
  }
  return [...rows.values()].map((row) => ({
    provider: row.provider,
    accounts: row.accounts.size,
    used: row.used,
    max: row.max,
    availability: [...row.availability],
    status: [...row.status],
    models: [...row.models],
  }));
}

export function ProviderSummary({ runners }: { readonly runners: readonly FleetRunner[] }): React.ReactElement {
  const rows = summarizeProviders(runners);
  return (
    <div className="min-w-0 overflow-x-auto rounded-xl" tabIndex={0} role="region" aria-label="Provider accounts">
      <table className="w-full min-w-[640px] text-left text-xs">
        <thead className="border-b border-border/60 text-muted-foreground">
          <tr>{["Provider", "Accounts", "In use", "Models", "Status"].map((heading) => <th key={heading} scope="col" className="px-4 py-3 font-medium">{heading}</th>)}</tr>
        </thead>
        <tbody className="divide-y divide-border/50">
          {rows.map((row) => {
            const { provider, used, max, status } = row;
            return (
              <tr key={provider}>
                <th scope="row" className="px-4 py-4 font-medium">{provider}</th>
                <td className="px-4 py-4 tabular-nums">{row.accounts}</td>
                <td className="min-w-28 px-4 py-4">
                  <span className="tabular-nums">{used}/{max}</span>
                  <div role="progressbar" aria-label={provider + " in use"} aria-valuenow={used} aria-valuemin={0} aria-valuemax={max} className="mt-2 h-1 w-16 overflow-hidden rounded-full bg-accent">
                    <div className="h-full bg-primary" style={{ width: (max > 0 ? Math.min(100, used / max * 100) : 0) + "%" }} />
                  </div>
                </td>
                <td className="max-w-56 px-4 py-4 text-muted-foreground">{row.models.join(", ") || "—"}</td>
                <td className="px-4 py-4">{status.map((value) => <p key={value} className={value.startsWith("Rate limited") ? "text-warning" : "text-muted-foreground"}>{value}</p>)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

const SLOT_TONES = {
  running: "border-primary bg-primary",
  free: "border-border bg-background",
  unavailable: "border-dashed border-warning bg-warning/10",
};

function Capacity({ runners, onOpen }: { readonly runners: readonly FleetRunner[]; readonly onOpen: (runner: FleetRunner) => void }): React.ReactElement {
  const machines = new Map<string, FleetRunner[]>();
  for (const runner of runners) {
    const group = machines.get(runner.machine_id) ?? [];
    group.push(runner);
    machines.set(runner.machine_id, group);
  }
  const hosts = [...machines.values()].map((group) => {
    const runner = group[0]!;
    const leases = [...new Map(group.flatMap((entry) => entry.leases.map((lease) => [lease.lease_id, lease] as const))).values()];
    const used = Math.max(leases.length, ...group.map((entry) => entry.host_used));
    const capacity = runner.host_capacity;
    const available = group.reduce((count, entry) => {
      if (entry.state !== "active" || (entry.health !== "online" && entry.health !== "asleep") || entry.claim_refusal_reason) return count;
      if (entry.provider_capacity.length > 0 && !entry.provider_capacity.some((provider) => provider.state !== "exhausted" && provider.used < provider.max_concurrent)) return count;
      return count + Math.max(0, Math.min(entry.capacity_limit, entry.reported_capacity) - entry.leases.length);
    }, 0);
    const free = Math.min(Math.max(0, capacity - used), available);
    return { runner, leases, used, capacity, free };
  });
  const used = hosts.reduce((count, host) => count + host.used, 0);
  const total = hosts.reduce((count, host) => count + host.capacity, 0);
  const unavailable = hosts.reduce((count, host) => count + Math.max(0, host.capacity - host.used - host.free), 0);
  return (
    <SettingsSection title="Capacity right now" variant="plain">
      <div className="space-y-5 rounded-xl border border-border/60 bg-card/40 p-4">
        <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
          <p className="text-base font-medium tabular-nums">{used} of {total} slots running work</p>
          {unavailable > 0 ? <p className="text-xs text-warning">{unavailable} slots can't take work</p> : null}
        </div>
        <div className="flex flex-wrap gap-x-8 gap-y-4">
          {hosts.map((host) => (
            <button key={host.runner.machine_id} type="button" onClick={() => onOpen(host.runner)} aria-label={"Open " + host.runner.display_name} className="min-w-0 max-w-full space-y-2 rounded-md text-left outline-none focus-visible:ring-2 focus-visible:ring-ring" data-testid="runner-capacity">
              <span className="block break-words text-xs text-muted-foreground">{host.runner.display_name}</span>
              <span className="flex flex-wrap gap-1.5">
                {Array.from({ length: host.capacity }, (_, index) => {
                  const lease = host.leases[index];
                  const state = index < host.used ? "running" : index < host.used + host.free ? "free" : "unavailable";
                  return <span key={index} data-slot-state={state} title={lease ? "#" + lease.work_item_id + " " + lease.title : state === "running" ? "Running work" : state === "free" ? "Free slot" : "Can't take work"} className={cn("size-5 shrink-0 rounded-sm border", SLOT_TONES[state])} />;
                })}
              </span>
            </button>
          ))}
        </div>
        <div className="flex flex-wrap gap-x-5 gap-y-2 text-xs text-muted-foreground" aria-label="Slot legend">
          {([["running", "Running work"], ["free", "Free"], ["unavailable", "Can't take work"]] as const).map(([state, label]) => (
            <span key={state} className="flex items-center gap-2"><span aria-hidden="true" className={cn("size-3 rounded-sm border", SLOT_TONES[state])} />{label}</span>
          ))}
        </div>
      </div>
    </SettingsSection>
  );
}

export function RunnersSectionView({
  fleet, now, organizationName = "this organization", onEnroll, enrollments = [], onSaveRouting, projects = [], onReloadRunner,
}: {
  readonly fleet: FleetResponse;
  readonly projects?: readonly RunnerProject[];
  readonly onReloadRunner?: () => Promise<void>;
  readonly now?: number;
  readonly organizationName?: string;
  readonly onEnroll?: (name?: string) => void;
  readonly enrollments?: readonly PendingEnrollment[];
  readonly onSaveRouting?: (runner: FleetRunner, routing: RunnerRouting) => Promise<void>;
}): React.ReactElement {
  const [attentionOnly, setAttentionOnly] = React.useState(() => new URLSearchParams(window.location.search).get("health") === "needs_attention");
  const attentionCount = fleet.runners.filter((runner) => runner.health === "needs_attention").length;
  const runners = attentionOnly ? fleet.runners.filter((runner) => runner.health === "needs_attention") : fleet.runners;
  function filterUrl(only: boolean): string {
    const url = new URL(window.location.href);
    if (only) url.searchParams.set("health", "needs_attention");
    else url.searchParams.delete("health");
    return url.pathname + url.search + url.hash;
  }
  function selectAttentionFilter(event: React.MouseEvent<HTMLAnchorElement>, only: boolean): void {
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    window.history.replaceState(window.history.state, "", filterUrl(only));
    setAttentionOnly(only);
  }
  React.useEffect(() => {
    const update = () => setAttentionOnly(new URLSearchParams(window.location.search).get("health") === "needs_attention");
    window.addEventListener("popstate", update);
    return () => window.removeEventListener("popstate", update);
  }, []);
  const [runnerToOpen, setRunnerToOpen] = React.useState<string | null>(null);
  function openRunner(runner: FleetRunner): void {
    if (attentionOnly && runner.health !== "needs_attention") {
      window.history.replaceState(window.history.state, "", filterUrl(false));
      setAttentionOnly(false);
    }
    setRunnerToOpen(runner.id);
  }
  const selectedRunner = fleet.runners.find((runner) => runner.id === runnerToOpen);
  return (
    <>
      {selectedRunner ? <RunnerDetailSheet key={selectedRunner.id} runner={selectedRunner} projects={projects} editable={fleet.editable !== false && onSaveRouting !== undefined} now={now} onClose={() => setRunnerToOpen(null)} onSave={onSaveRouting} onReload={onReloadRunner} /> : null}
      <header className="flex flex-col items-start justify-between gap-4 sm:flex-row">
        <div className="min-w-0 space-y-2">
          <h2 className="text-xl font-semibold tracking-tight">Providers & runners</h2>
          <p className="text-[13px] text-muted-foreground">The machines that take work for {organizationName}, and the provider accounts each one can reach.</p>
        </div>
        {onEnroll === undefined || fleet.runners.length === 0 ? null : <Button size="sm" className="shrink-0" onClick={() => onEnroll()}>Enroll a runner</Button>}
      </header>
      {fleet.runners.length === 0 ? (
        <div className="flex flex-col items-start gap-4 rounded-xl border border-border/60 bg-card/40 p-6">
          <p className="text-sm">No runners yet. Enroll a machine to start taking work.</p>
          {onEnroll === undefined ? null : <Button size="sm" onClick={() => onEnroll()}>Enroll a runner</Button>}
        </div>
      ) : (
        <>
          <Capacity runners={fleet.runners} onOpen={openRunner} />
          {fleet.runners.filter((runner) => runner.health === "needs_attention" || runner.claim_refusal_reason).map((runner) => (
            <div key={runner.id} role="alert" data-testid={runner.health === "needs_attention" ? "runner-attention" : "host-update"} className="flex min-w-0 items-start gap-3 rounded-xl border border-warning/30 bg-warning/8 p-4">
              <AlertTriangleIcon aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-warning" />
              <div className="min-w-0 flex-1 space-y-2 text-xs">
                <p className="text-sm font-medium">{runner.display_name} can't take work</p>
                {runner.sprite?.wake_failed ? <p>The Hub could not wake this Sprite. Check the Sprite and its project’s Sprites token.</p> : runner.sprite && !runner.sprite.can_wake ? <p>No Sprites token is set for an accessible project; the Hub cannot wake this Sprite.</p> : null}
                {runner.health === "needs_attention" && runner.problems?.[0] ? <div className="space-y-1"><p>{runner.problems[0].message}</p><p className="text-muted-foreground">{runner.problems[0].fix_hint}</p></div> : null}
                {runner.claim_refusal_reason ? <div data-testid={runner.health === "needs_attention" ? "host-update" : undefined} className="space-y-2"><p>{runner.claim_refusal_reason}</p><PathValue value={RUNNER_UPGRADE_COMMAND} /></div> : null}
                <button type="button" className="rounded text-warning underline underline-offset-4 focus-visible:outline-2 focus-visible:outline-ring" onClick={() => openRunner(runner)}>Open runner<span className="sr-only"> {runner.display_name}</span></button>
              </div>
            </div>
          ))}
          <SettingsSection id="settings-runners" title="Runners" variant="plain">
            <nav aria-label="Filter runners" className="mb-3 flex w-fit max-w-full rounded-lg border border-border/60 bg-muted/40 p-1 text-xs">
              {([[false, "All " + fleet.runners.length], [true, "Needs attention " + attentionCount]] as const).map(([only, label]) => (
                <a key={label} href={filterUrl(only)} aria-current={attentionOnly === only ? "true" : undefined} onClick={(event) => selectAttentionFilter(event, only)} className={cn("rounded-md px-3 py-1.5 outline-none focus-visible:ring-2 focus-visible:ring-ring", attentionOnly === only ? "bg-background text-foreground shadow-xs" : "text-muted-foreground hover:text-foreground")}>{label}</a>
              ))}
            </nav>
            <div className="overflow-hidden rounded-xl border border-border/60 bg-card/40">
              <div aria-hidden="true" className="hidden grid-cols-[minmax(0,1fr)_minmax(0,1fr)_7rem_auto] gap-4 border-b border-border/50 px-4 py-3 text-xs text-muted-foreground sm:grid"><span>Runner</span><span>Running</span><span>Last check-in</span><span className="w-14" /></div>
              <div className="divide-y divide-border/50">
                {runners.map((runner) => <HostCard key={runner.id} runner={runner} now={now} onOpen={() => openRunner(runner)} />)}
                {runners.length === 0 ? <p className="px-4 py-6 text-[13px] text-muted-foreground">No runners need attention.</p> : null}
              </div>
            </div>
          </SettingsSection>
        </>
      )}
      {enrollments.length === 0 ? null : (
        <SettingsSection id="settings-enrollments" title="Waiting to connect" icon={<ServerIcon className="size-3.5" />} headerAction={<span className="text-xs text-muted-foreground">Created in this session</span>}>
          <PendingEnrollments enrollments={enrollments} onRenew={onEnroll} />
        </SettingsSection>
      )}
      <SettingsSection id="settings-providers" title="Providers" icon={<BotIcon className="size-3.5" />} className="min-w-0">
        {fleet.runners.some((runner) => runner.provider_capacity.length > 0) ? <ProviderSummary runners={fleet.runners} /> : <SettingsRow title="No provider capacity reported" description="A runner reports the providers it can reach when it heartbeats." />}
      </SettingsSection>
    </>
  );
}

/** The section as the settings page mounts it: it owns the one read. */
export function RunnersSettings(): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const fleet = useResource(() => api.fleet(), [api]);
  const [open, setOpen] = React.useState(false);
  const [enrollmentName, setEnrollmentName] = React.useState("");
  const [enrollments, setEnrollments] = React.useState<readonly PendingEnrollment[]>([]);
  const canEnroll = bootstrap?.actor.can_manage_runners ?? false;
  async function reloadRunner(): Promise<void> {
    fleet.set(await api.fleet());
  }

  return (
    <SettingsPageContainer>
      {fleet.value === undefined ? (
        fleet.error !== null ? (
          <SettingsSection
            title="Providers & runners"
            icon={<ServerIcon className="size-3.5" />}
          >
            <SettingsRow
              title="The fleet is not available"
              description={
                fleet.error.isAccessError
                  ? "You do not have access to this organization's fleet."
                  : fleet.error.message
              }
              control={
                <Button size="sm" variant="outline" onClick={() => void fleet.refresh()}>
                  Try again
                </Button>
              }
            />
          </SettingsSection>
        ) : (
          <SettingsSection
            title="Providers & runners"
            icon={<ServerIcon className="size-3.5" />}
          >
            <SettingsRow title="Loading the fleet" />
          </SettingsSection>
        )
      ) : (
        <RunnersSectionView
          fleet={fleet.value}
          organizationName={bootstrap?.organization.name}
          projects={bootstrap?.projects ?? []}
          onReloadRunner={reloadRunner}
          enrollments={enrollments}
          {...(canEnroll ? { onEnroll: (name = "") => { setEnrollmentName(name); setOpen(true); } } : {})}
          {...(canEnroll && fleet.value.editable ? { onSaveRouting: async (runner: FleetRunner, routing: RunnerRouting) => {
            await api.setRunnerRouting({ runner: runner.id, revision: runner.revision ?? 0, displayName: routing.display_name,
              tags: routing.tags, state: routing.state, capacityLimit: routing.capacity_limit, projectIds: routing.project_ids,
              homeProjectIds: routing.home_project_ids ?? [], isolationTier: routing.isolation_tier, hostServices: routing.host_services, availability: routing.availability,
              spillover: routing.spillover });
            await reloadRunner();
          } } : {})}
        />
      )}
      {canEnroll ? (
        <EnrollRunnerDialog
          open={open}
          initialName={enrollmentName}
          onOpenChange={setOpen}
          fleet={fleet}
          onConnected={(entry) => setEnrollments((current) => current.filter((old) => old.id !== entry.id))}
          onEnrolled={(entry) => {
            setEnrollments((current) => [entry, ...current.filter((old) => old.id !== entry.id)]);
            // A redeemed enrollment shows up as a runner, not as an
            // enrollment, so the fleet is re-read rather than guessed at.
            void fleet.refresh();
          }}
        />
      ) : null}
    </SettingsPageContainer>
  );
}
