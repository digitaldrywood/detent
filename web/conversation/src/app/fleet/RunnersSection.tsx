import { BotIcon, ServerIcon } from "lucide-react";
import React from "react";

import { Button } from "../../components/ui/button.tsx";
import type { FleetResponse, FleetRunner, ProviderCapacity } from "../../contracts/account.ts";
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

export function RunnersSectionView({
  fleet,
  now,
  onEnroll,
  enrollments = [],
  organizationId = "",
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
  readonly organizationId?: string;
}): React.ReactElement {
  const leases = fleet.runners.reduce((count, runner) => count + runner.leases.length, 0);
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
          <span className="flex items-center gap-3">
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
            {fleet.runners.map((runner) => (
              <HostCard
                key={runner.id}
                runner={runner}
                now={now}
                current={fleet.current ?? ""}
              />
            ))}
          </div>
        )}
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
          <PendingEnrollments enrollments={enrollments} organizationId={organizationId} />
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
          organizationId={bootstrap?.organization.id ?? ""}
          {...(canEnroll ? { onEnroll: () => setOpen(true) } : {})}
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
