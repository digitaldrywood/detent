import { BotIcon, ChevronDownIcon, ServerIcon } from "lucide-react";
import React from "react";
import { useNavigate } from "@tanstack/react-router";
import { Menu, MenuTrigger, MenuPopup, MenuItem } from "../../components/ui/menu.tsx";
import { accountKey, readLastProject, useClient } from "../client.ts";
import { SpritePoolCard } from "../account/SpritePoolCard.tsx";

import { Button } from "../../components/ui/button.tsx";
import { Dialog, DialogDescription, DialogFooter, DialogHeader, DialogPanel, DialogPopup, DialogTitle } from "../../components/ui/dialog.tsx";
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
import { NativeSelect } from "../account/controls.tsx";
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
      accounts: Map<string, { used: number; max: number }>;
      availability: Set<string>;
      status: Set<string>;
      models: Set<string>;
    }
  >();
  const capacities: ProviderCapacity[] = runners.flatMap((runner) => [...runner.provider_capacity]);
  const sharedProviders = new Set(capacities.filter((capacity) => !capacity.shared_account_alias).map((capacity) => capacity.provider));
  for (const capacity of capacities) {
    let row = rows.get(capacity.provider);
    if (row === undefined) {
      row = {
        provider: capacity.provider,
        accounts: new Map(),
        availability: new Set(),
        status: new Set(),
        models: new Set(),
      };
      rows.set(capacity.provider, row);
    }
    const account = sharedProviders.has(capacity.provider) ? "" : capacity.shared_account_alias ?? "";
    const pool = row.accounts.get(account);
    const max = capacity.max_concurrent ?? 0;
    row.accounts.set(account, {
      used: Math.max(pool?.used ?? 0, capacity.used),
      max: pool?.max && max ? Math.min(pool.max, max) : pool?.max || max,
    });
    row.availability.add(capacity.availability);
    row.status.add(capacity.reset_at ? "Rate limited until " + formatLocalTime(capacity.reset_at)
      : [capacity.availability, capacity.state].filter(Boolean).map((value) => value.replaceAll("_", " ")).join(" · "));
    for (const model of capacity.models ?? []) row.models.add(model);
  }
  return [...rows.values()].map((row) => ({
    provider: row.provider,
    accounts: row.accounts.size,
    used: [...row.accounts.values()].reduce((sum, pool) => sum + pool.used, 0),
    max: [...row.accounts.values()].reduce((sum, pool) => sum + pool.max, 0),
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
                <td className="px-4 py-4">{status.map((value) => <p key={value} className={value.startsWith("Rate limited") ? "text-warning-foreground" : "text-muted-foreground"}>{value}</p>)}</td>
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
      if (entry.provider_capacity.length > 0 && !entry.provider_capacity.some((provider) => provider.state !== "exhausted" && (provider.max_concurrent === undefined || provider.max_concurrent === 0 || provider.used < provider.max_concurrent))) return count;
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
          {unavailable > 0 ? <p className="text-xs text-warning-foreground">{unavailable} slots can't take work</p> : null}
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
  fleet, now, organizationName = "this organization", onEnroll, enrollments = [], onSaveRouting, projects = [], onReloadRunner, onRemoveRunner, onAssist, spritePools, spritePoolAttentionCount = 0,
}: {
  readonly fleet: FleetResponse;
  readonly onAssist?: () => void;
  readonly spritePools?: (attentionOnly: boolean) => React.ReactNode;
  readonly spritePoolAttentionCount?: number;
  readonly projects?: readonly RunnerProject[];
  readonly onReloadRunner?: () => Promise<void>;
  readonly now?: number;
  readonly organizationName?: string;
  readonly onEnroll?: (name?: string) => void;
  readonly enrollments?: readonly PendingEnrollment[];
  readonly onSaveRouting?: (runner: FleetRunner, routing: RunnerRouting) => Promise<void>;
  readonly onRemoveRunner?: (runner: FleetRunner) => Promise<void>;
}): React.ReactElement {
  const [attentionOnly, setAttentionOnly] = React.useState(() => new URLSearchParams(window.location.search).get("health") === "needs_attention");
  const attentionCount = fleet.runners.filter((runner) => runner.health === "needs_attention").length + spritePoolAttentionCount;
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
  const [runnerToRemove, setRunnerToRemove] = React.useState<FleetRunner | null>(null);
  const [removing, setRemoving] = React.useState(false);
  const [removeError, setRemoveError] = React.useState<string | null>(null);
  async function removeRunner(): Promise<void> {
    if (!runnerToRemove || !onRemoveRunner || removing) return;
    setRemoving(true);
    setRemoveError(null);
    try {
      await onRemoveRunner(runnerToRemove);
      setRunnerToRemove(null);
    } catch (error) {
      setRemoveError(error instanceof Error ? error.message : "Could not remove the runner.");
    } finally {
      setRemoving(false);
    }
  }
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
      <Dialog open={runnerToRemove !== null} onOpenChange={(open) => { if (!open && !removing) setRunnerToRemove(null); }}>
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>Remove runner {runnerToRemove?.display_name}?</DialogTitle>
            <DialogDescription>This revokes its credentials and removes it from the fleet. Historical runs keep the runner's name. Runners with active work must finish their attempts before removal.</DialogDescription>
          </DialogHeader>
          {removeError ? <DialogPanel><p role="alert" className="text-sm text-destructive">{removeError}</p></DialogPanel> : null}
          <DialogFooter>
            <Button variant="outline" disabled={removing} onClick={() => setRunnerToRemove(null)}>Cancel</Button>
            <Button variant="destructive" disabled={removing} onClick={() => void removeRunner()}>{removing ? "Removing…" : "Remove runner"}</Button>
          </DialogFooter>
        </DialogPopup>
      </Dialog>
      {selectedRunner ? <RunnerDetailSheet key={selectedRunner.id} runner={selectedRunner} projects={projects} editable={fleet.editable !== false && onSaveRouting !== undefined} now={now} onClose={() => setRunnerToOpen(null)} onSave={onSaveRouting} onReload={onReloadRunner} /> : null}
      <header className="flex flex-col items-start justify-between gap-4 sm:flex-row">
        <div className="min-w-0 space-y-2">
          <h2 className="text-xl font-semibold tracking-tight">Providers & runners</h2>
          <p className="text-[13px] text-muted-foreground">Runners take work for {organizationName}. Add capacity manually, or let Luna walk you through setup.</p>
        </div>
        {onEnroll === undefined ? null : <Menu>
          <MenuTrigger render={<Button size="sm">Add runner <ChevronDownIcon aria-hidden="true" /></Button>} />
          <MenuPopup align="end">
            <MenuItem onClick={() => onEnroll()}>Manual</MenuItem>
            <MenuItem disabled={onAssist === undefined} onClick={onAssist}>AI assisted</MenuItem>
          </MenuPopup>
        </Menu>}
      </header>
      {fleet.runners.length === 0 ? (
        <div className="flex flex-col items-start gap-4 rounded-xl border border-border/60 bg-card/40 p-6">
          <p className="text-sm">No runners yet. Add a runner manually or let Luna help you set one up.</p>
        </div>
      ) : (
        <>
          <SettingsSection id="settings-runners" title="Runners" variant="plain">
            <nav aria-label="Filter runners" className="mb-3 flex w-fit max-w-full rounded-lg border border-border/60 bg-muted/40 p-1 text-xs">
              {([[false, "All " + fleet.runners.length], [true, "Needs attention " + attentionCount]] as const).map(([only, label]) => (
                <a key={label} href={filterUrl(only)} aria-current={attentionOnly === only ? "true" : undefined} onClick={(event) => selectAttentionFilter(event, only)} className={cn("rounded-md px-3 py-1.5 outline-none focus-visible:ring-2 focus-visible:ring-ring", attentionOnly === only ? "bg-background text-foreground shadow-xs" : "text-muted-foreground hover:text-foreground")}>{label}</a>
              ))}
            </nav>
            <div className="@container/runner-list overflow-hidden rounded-xl border border-border/60 bg-card/40">
              <div aria-hidden="true" className="hidden grid-cols-[minmax(0,1.2fr)_6rem_minmax(0,1fr)_7rem_minmax(0,1fr)_11rem] gap-4 border-b border-border/50 px-4 py-3 text-xs text-muted-foreground @[48rem]/runner-list:grid"><span>Runner</span><span>Host kind</span><span>Health</span><span>In use</span><span>Projects</span><span /></div>
              <div className="divide-y divide-border/50">
                {runners.map((runner) => <HostCard key={runner.id} runner={runner} projects={projects} onOpen={() => openRunner(runner)} {...(fleet.editable !== false && onRemoveRunner ? { onRemove: () => { setRemoveError(null); setRunnerToRemove(runner); } } : {})} />)}
                {runners.length === 0 ? <p className="px-4 py-6 text-[13px] text-muted-foreground">No runners need attention.</p> : null}
              </div>
              {spritePools?.(attentionOnly)}
            </div>
          </SettingsSection>
        </>
      )}
      {fleet.runners.length === 0 ? spritePools?.(false) : null}
      {enrollments.length === 0 ? null : (
        <SettingsSection id="settings-enrollments" title="Waiting to connect" icon={<ServerIcon className="size-3.5" />} headerAction={<span className="text-xs text-muted-foreground">Created in this session</span>}>
          <PendingEnrollments enrollments={enrollments} onRenew={onEnroll} />
        </SettingsSection>
      )}
      {fleet.runners.length > 0 ? <details className="space-y-4">
        <summary className="cursor-pointer text-sm text-muted-foreground">Fleet capacity and providers</summary>
        <Capacity runners={fleet.runners} onOpen={openRunner} />
        <SettingsSection id="settings-providers" title="Providers" icon={<BotIcon className="size-3.5" />} className="min-w-0">
          {fleet.runners.some((runner) => runner.provider_capacity.length > 0) ? <ProviderSummary runners={fleet.runners} /> : <SettingsRow title="No provider capacity reported" description="A runner reports the providers it can reach when it heartbeats." />}
        </SettingsSection>
      </details> : null}
    </>
  );
}

/** The section as the settings page mounts it: it owns the one read. */
export function RunnersSettings(): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const client = useClient();
  const navigate = useNavigate();
  const [assistOpen, setAssistOpen] = React.useState(false);
  const [poolAttention, setPoolAttention] = React.useState<Readonly<Record<string, boolean>>>({});
  const onPoolAttentionChange = React.useCallback((projectId: string, needsAttention: boolean) => {
    setPoolAttention((current) => current[projectId] === needsAttention ? current : { ...current, [projectId]: needsAttention });
  }, []);
  const writableProjects = bootstrap?.projects.filter((project) => project.can_write) ?? [];
  const [assistProject, setAssistProject] = React.useState(() => writableProjects.find((project) => project.id === readLastProject())?.id ?? writableProjects[0]?.id ?? "");
  function assist(projectId: string): void {
    const scope = { accountKey: accountKey(client), projectId, conversationId: "new" };
    const request = "Help me add a runner for this project. Ask whether I want a Fly Sprite or a machine I run, then walk me through enrollment, the register command, provider login and the first connection.";
    const existing = client.drafts.readDraft(scope);
    client.drafts.writeDraft(scope, existing.trim() ? `${existing}\n\n${request}` : request);
    setAssistOpen(false);
    void navigate({ to: "/chat/p/$projectId", params: { projectId } });
  }
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
          spritePoolAttentionCount={bootstrap?.projects.filter((project) => poolAttention[project.id]).length ?? 0}
          spritePools={(attentionOnly) => bootstrap?.projects.map((project) => <SpritePoolCard key={project.id} compact projectId={project.id} projectName={project.name} canManage={bootstrap.actor.can_manage && project.can_write} attentionOnly={attentionOnly} onAttentionChange={onPoolAttentionChange} />)}
          {...(canEnroll && writableProjects.length > 0 ? { onAssist: () => {
            if (writableProjects.length === 1) assist(writableProjects[0]!.id);
            else setAssistOpen(true);
          } } : {})}
          {...(canEnroll && fleet.value.editable ? { onRemoveRunner: async (runner: FleetRunner) => {
            await api.removeRunner(runner.id);
            fleet.set({ ...fleet.value!, runners: fleet.value!.runners.filter((entry) => entry.id !== runner.id) });
            await fleet.refresh();
          } } : {})}
          enrollments={enrollments}
          {...(canEnroll ? { onEnroll: (name = "") => { setEnrollmentName(name); setOpen(true); } } : {})}
          {...(canEnroll && fleet.value.editable ? { onSaveRouting: async (runner: FleetRunner, routing: RunnerRouting) => {
            await api.setRunnerRouting({ runner: runner.id, revision: runner.revision ?? 0, displayName: routing.display_name,
              tags: routing.tags, state: routing.state, capacityLimit: routing.capacity_limit, projectIds: routing.project_ids,
              isolationTier: routing.isolation_tier, hostServices: routing.host_services, availability: routing.availability });
            await reloadRunner();
          } } : {})}
        />
      )}
      <Dialog open={assistOpen} onOpenChange={setAssistOpen}>
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>Add runner with Luna</DialogTitle>
            <DialogDescription>Luna will help you choose a Fly Sprite or your own machine and walk you through setup.</DialogDescription>
          </DialogHeader>
          <DialogPanel>
            <label className="grid gap-2 text-sm">Project
              <NativeSelect aria-label="Project" value={assistProject} onValueChange={setAssistProject} options={writableProjects.map((project) => ({ value: project.id, label: project.name }))} />
            </label>
          </DialogPanel>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAssistOpen(false)}>Cancel</Button>
            <Button disabled={!writableProjects.some((project) => project.id === assistProject)} onClick={() => assist(assistProject)}>Open Luna</Button>
          </DialogFooter>
        </DialogPopup>
      </Dialog>
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
