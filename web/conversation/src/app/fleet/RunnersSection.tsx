import { BotIcon, ServerIcon } from "lucide-react";
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
import { SettingsHelp } from "../settings/SettingsHelp.tsx";
import { RUNNER_HELP } from "./runnerHelp.ts";
import { HostCard } from "./HostCard.tsx";

// --- Providers --------------------------------------------------------------

const DOT_TONES = ["bg-primary", "bg-info", "bg-success", "bg-warning"] as const;

export interface ProviderRow {
  readonly provider: string;
  readonly accounts: number;
  readonly used: number;
  readonly max: number;
  readonly availability: readonly string[];
  readonly models: readonly string[];
}

/** Every runner's provider capacity, folded onto one row per provider. */
export function summarizeProviders(runners: readonly FleetRunner[]): readonly ProviderRow[] {
  const rows = new Map<
    string,
    {
      provider: string;
      accounts: Set<string>;
      used: number;
      max: number;
      availability: Set<string>;
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
        models: new Set(),
      };
      rows.set(capacity.provider, row);
    }
    row.accounts.add(capacity.account_alias);
    row.used += capacity.used;
    row.max += capacity.max_concurrent;
    row.availability.add(capacity.availability);
    for (const model of capacity.models ?? []) row.models.add(model);
  }
  return [...rows.values()].map((row) => ({
    provider: row.provider,
    accounts: row.accounts.size,
    used: row.used,
    max: row.max,
    availability: [...row.availability],
    models: [...row.models],
  }));
}

export function ProviderSummary({
  runners,
}: {
  readonly runners: readonly FleetRunner[];
}): React.ReactElement | null {
  const rows = summarizeProviders(runners);
  if (rows.length === 0) return null;
  return (
    <>
      {rows.map((row, index) => (
        <SettingsRow
          key={row.provider}
          help={{ label: `${row.provider} provider capacity`, text: RUNNER_HELP.provider }}
          title={
            <span className="flex min-w-0 items-center gap-2">
              <span
                aria-hidden="true"
                className={cn(
                  "size-2 shrink-0 rounded-full",
                  DOT_TONES[index % DOT_TONES.length] ?? "bg-primary",
                )}
              />
              <span className="truncate">{row.provider}</span>
            </span>
          }
          description={`${row.accounts} ${row.accounts === 1 ? "account" : "accounts"} · ${row.availability.join(", ")}`}
          status={row.models.length > 0 ? row.models.join(", ") : undefined}
          control={
            <span className="text-[13px] tabular-nums">
              {row.used}/{row.max} in use
            </span>
          }
        />
      ))}
    </>
  );
}

// --- Section ----------------------------------------------------------------

function RunnerSettingsForm({
  runner,
  onSave,
}: {
  readonly runner: FleetRunner;
  readonly onSave: (runner: FleetRunner, routing: RunnerRouting) => Promise<void>;
}): React.ReactElement | null {
  const routing = runner.routing;
  const [error, setError] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  if (routing === undefined) return null;
  const field = (data: FormData, name: string): string => String(data.get(name) ?? "").trim();
  const lines = (value: string): string[] => value.split(/[\n,]/).map((part) => part.trim()).filter(Boolean);

  async function submit(event: React.FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (routing === undefined) return;
    const data = new FormData(event.currentTarget);
    const next: RunnerRouting = {
      display_name: field(data, "display_name"),
      tags: lines(field(data, "tags")),
      state: field(data, "state"),
      capacity_limit: Number(field(data, "capacity_limit")),
      project_ids: lines(field(data, "project_ids")),
      home_project_ids: lines(field(data, "home_project_ids")),
      isolation_tier: field(data, "isolation_tier"),
      host_services: field(data, "host_services").split("\n").map((part) => part.trim()).filter(Boolean),
      availability: {
        timezone: field(data, "timezone"),
        windows: lines(field(data, "windows")),
        hard_deadline: field(data, "hard_deadline"),
      },
      spillover: {
        mode: field(data, "spillover_mode"),
        after_minutes: field(data, "spillover_mode") === "never" ? 0 : Number(field(data, "after_minutes")),
      },
    };
    setSaving(true);
    setError("");
    try {
      await onSave(runner, next);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Runner settings could not be saved.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <details className="border-t border-border/60 pt-2 text-xs">
      <summary className="cursor-pointer font-medium">Edit runner settings</summary>
      <form className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2" onSubmit={(event) => void submit(event)}>
        <label>Runner name<input className="mt-1 w-full rounded border p-2" name="display_name" defaultValue={routing.display_name} required /></label>
        <div>
          <div className="flex items-center gap-1.5">
            <label htmlFor={`tags-${runner.id}`}>Tags</label>
            <SettingsHelp label="Tags">{RUNNER_HELP.tags}</SettingsHelp>
          </div>
          <input id={`tags-${runner.id}`} className="mt-1 w-full rounded border p-2" name="tags" defaultValue={routing.tags.join(", ")} />
        </div>
        <label>State<select className="mt-1 w-full rounded border p-2" name="state" defaultValue={routing.state}><option value="active">Active</option><option value="draining">Draining</option><option value="disabled">Disabled</option></select></label>
        <div>
          <div className="flex items-center gap-1.5">
            <label htmlFor={`capacity_limit-${runner.id}`}>Runner capacity limit</label>
            <SettingsHelp label="Runner capacity limit">{RUNNER_HELP.limit}</SettingsHelp>
          </div>
          <input id={`capacity_limit-${runner.id}`} className="mt-1 w-full rounded border p-2" name="capacity_limit" type="number" min="0" max="10000" defaultValue={routing.capacity_limit} required />
        </div>
        <div>
          <div className="flex items-center gap-1.5">
            <label htmlFor={`project_ids-${runner.id}`}>Authorized project IDs</label>
            <SettingsHelp label="Authorized project IDs">{RUNNER_HELP.projects}</SettingsHelp>
          </div>
          <input id={`project_ids-${runner.id}`} className="mt-1 w-full rounded border p-2" name="project_ids" defaultValue={routing.project_ids.join(", ")} />
        </div>
        <label>Home project IDs<input className="mt-1 w-full rounded border p-2" name="home_project_ids" defaultValue={(routing.home_project_ids ?? []).join(", ")} /></label>
        <label>Isolation tier<select className="mt-1 w-full rounded border p-2" name="isolation_tier" defaultValue={routing.isolation_tier}><option value="sandbox">Sandbox</option><option value="native-trusted">Trusted only: full host access</option></select></label>
        <label className="sm:col-span-2">Host services, one per line<textarea className="mt-1 w-full rounded border p-2" name="host_services" rows={2} defaultValue={routing.host_services.join("\n")} placeholder="tcp:127.0.0.1:8080" /></label>
        <label>Availability timezone<input className="mt-1 w-full rounded border p-2" name="timezone" defaultValue={routing.availability.timezone} placeholder="America/Chicago" /></label>
        <label>Hard deadline after window closes<input className="mt-1 w-full rounded border p-2" name="hard_deadline" defaultValue={routing.availability.hard_deadline} placeholder="30m" /></label>
        <label className="sm:col-span-2">Weekly windows, one per line<textarea className="mt-1 w-full rounded border p-2" name="windows" rows={2} defaultValue={routing.availability.windows.join("\n")} placeholder="Mon-Fri 09:00-17:00" /></label>
        <div><label htmlFor={`spillover-${runner.id}`}>Spillover</label><select id={`spillover-${runner.id}`} className="mt-1 w-full rounded border p-2" name="spillover_mode" defaultValue={routing.spillover.mode}><option value="never">Never</option><option value="after">After waiting</option></select></div>
        <label>Spillover wait in minutes<input className="mt-1 w-full rounded border p-2" name="after_minutes" type="number" min="0" defaultValue={routing.spillover.after_minutes} /></label>
        {error === "" ? null : <p role="alert" className="text-destructive sm:col-span-2">{error}</p>}
        <div className="sm:col-span-2"><Button type="submit" size="sm" disabled={saving}>{saving ? "Saving…" : "Save runner"}</Button></div>
      </form>
    </details>
  );
}

export function RunnersSectionView({
  fleet,
  now,
  onEnroll,
  enrollments = [],
  onSaveRouting,
}: {
  readonly fleet: FleetResponse;
  readonly now?: number;
  /**
   * Opens the enrollment dialog. Absent for a reader the hub would refuse:
   * enrollment needs the runner grant on every project in the organization
   * (`hostedAllRunnerGrants`), so offering the control to anybody else would
   * be a button that can only produce a 404.
   */
  readonly onEnroll?: () => void;
  /** Enrollments this screen created, newest first. */
  readonly enrollments?: readonly PendingEnrollment[];
  readonly onSaveRouting?: (runner: FleetRunner, routing: RunnerRouting) => Promise<void>;
}): React.ReactElement {
  const leases = fleet.runners.reduce((count, runner) => count + runner.leases.length, 0);
  const [attentionOnly, setAttentionOnly] = React.useState(() => new URLSearchParams(window.location.search).get("health") === "needs_attention");
  const attentionCount = fleet.runners.filter((runner) => runner.health === "needs_attention").length;
  const runners = attentionOnly ? fleet.runners.filter((runner) => runner.health === "needs_attention") : fleet.runners;
  function selectAttentionFilter(event: React.MouseEvent<HTMLAnchorElement>, only: boolean): void {
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    const url = new URL(window.location.href);
    if (only) url.searchParams.set("health", "needs_attention");
    else url.searchParams.delete("health");
    window.history.replaceState(window.history.state, "", url);
    setAttentionOnly(only);
  }
  return (
    <>
      <SettingsSection
        id="settings-providers"
        title="Providers"
        icon={<BotIcon className="size-3.5" />}
      >
        {fleet.runners.some((runner) => runner.provider_capacity.length > 0) ? (
          <ProviderSummary runners={fleet.runners} />
        ) : (
          <SettingsRow
            title="No provider capacity reported"
            description="A runner reports the providers it can reach when it heartbeats."
          />
        )}
      </SettingsSection>

      <SettingsSection
        id="settings-runners"
        title="Runners"
        icon={<ServerIcon className="size-3.5" />}
        headerAction={
          <span className="flex flex-wrap items-center gap-3">
            {attentionCount === 0 ? null : (
              <a className="text-xs text-warning underline" href="?health=needs_attention#settings-runners" onClick={(event) => selectAttentionFilter(event, true)}>
                {attentionCount} {attentionCount === 1 ? "runner needs" : "runners need"} attention
              </a>
            )}
            {attentionOnly ? <a className="text-xs underline" href="?health=all#settings-runners" onClick={(event) => selectAttentionFilter(event, false)}>All runners</a> : null}
            <span className="text-xs text-muted-foreground tabular-nums">
              {fleet.runners.length} {fleet.runners.length === 1 ? "runner" : "runners"} · {leases}{" "}
              active {leases === 1 ? "lease" : "leases"}
            </span>
            {onEnroll === undefined ? null : (
              <Button size="xs" variant="outline" onClick={onEnroll}>
                Enroll a runner
              </Button>
            )}
          </span>
        }
        variant="plain"
      >
        {fleet.runners.length === 0 ? (
          <p className="px-3 py-6 text-[13px] text-muted-foreground sm:px-4">
            No runners are enrolled on this organization yet.
          </p>
        ) : (
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            {runners.map((runner) => (
              <HostCard
                key={runner.id}
                runner={runner}
                now={now}
                settings={onSaveRouting === undefined || runner.routing === undefined ? undefined : <RunnerSettingsForm key={runner.revision} runner={runner} onSave={onSaveRouting} />}
              />
            ))}
          </div>
        )}
        {attentionOnly && fleet.runners.length > 0 && runners.length === 0 ? <p className="px-3 py-6 text-[13px] text-muted-foreground sm:px-4">No runners need attention.</p> : null}
      </SettingsSection>

      {enrollments.length === 0 ? null : (
        <SettingsSection
          id="settings-enrollments"
          title="Pending enrollments"
          icon={<ServerIcon className="size-3.5" />}
          headerAction={
            <span className="text-xs text-muted-foreground">Created in this session</span>
          }
        >
          <PendingEnrollments enrollments={enrollments} />
        </SettingsSection>
      )}
    </>
  );
}

/** The section as the settings page mounts it: it owns the one read. */
export function RunnersSettings(): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const fleet = useResource(() => api.fleet(), [api]);
  const [open, setOpen] = React.useState(false);
  const [enrollments, setEnrollments] = React.useState<readonly PendingEnrollment[]>([]);
  const canEnroll = bootstrap?.actor.can_manage_runners ?? false;

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
          enrollments={enrollments}
          {...(canEnroll ? { onEnroll: () => setOpen(true) } : {})}
          {...(canEnroll && fleet.value.editable ? { onSaveRouting: async (runner: FleetRunner, routing: RunnerRouting) => {
            await api.setRunnerRouting({ runner: runner.id, revision: runner.revision ?? 0, displayName: routing.display_name,
              tags: routing.tags, state: routing.state, capacityLimit: routing.capacity_limit, projectIds: routing.project_ids,
              homeProjectIds: routing.home_project_ids ?? [], isolationTier: routing.isolation_tier, hostServices: routing.host_services, availability: routing.availability,
              spillover: routing.spillover });
            await fleet.refresh();
          } } : {})}
        />
      )}
      {canEnroll ? (
        <EnrollRunnerDialog
          open={open}
          onOpenChange={setOpen}
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
