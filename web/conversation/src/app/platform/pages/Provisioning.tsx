import { Link } from "@tanstack/react-router";
import React from "react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import type { PlatformTenants } from "../../../contracts/platform.ts";
import { useResource } from "../../account/useResource.ts";
import type { PlatformHealth } from "../../entry/api.ts";
import { formatBytes } from "../../entry/PlatformConsole.tsx";
import { SettingsRow, SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { Ago, EmptyNote, Loadable, PlatformPage, StateLabel } from "../ui.tsx";

function Meter({ used, limit, floor = false }: { readonly used: number; readonly limit: number; readonly floor?: boolean }) {
  const ratio = limit <= 0 ? 0 : Math.min(used / limit, 1);
  const tight = floor ? used < limit * 1.25 : ratio >= 0.85;
  return (
    <div className="h-1 w-full overflow-hidden rounded-full bg-muted" aria-hidden="true">
      <div
        className={tight ? "h-full rounded-full bg-warning" : "h-full rounded-full bg-primary"}
        style={{ width: `${Math.round((floor ? Math.min(limit / Math.max(used, 1), 1) : ratio) * 100)}%` }}
      />
    </div>
  );
}

/** The entry's admission readings, as settings rows. Live today. */
export function CapacityRows({ health }: { readonly health: PlatformHealth }): React.ReactElement {
  const admission = health.admission;
  return (
    <>
      <SettingsRow className="text-sm"
        title="Registry"
        control={<StateLabel state={health.registry.ok ? "ready" : "failed"} label={health.registry.ok ? "Reachable" : "Unavailable"} />}
      />
      {health.tenants === undefined ? null : (
        <SettingsRow className="text-sm"
          title="Tenant Hubs running"
          description="Ready organizations whose Hub answers its health check."
          control={
            <span className="tabular-nums">
              {health.tenants.running} of {health.tenants.expected}
            </span>
          }
        />
      )}
      {admission === undefined ? (
        <SettingsRow className="text-sm" title="Admission" description="Self-service allocation is not configured, so there are no admission limits." />
      ) : (
        <>
          <SettingsRow className="text-sm" title="Tenant slots" control={<span className="tabular-nums">{admission.tenants} of {admission.max_tenants}</span>}>
            <div className="pb-3">
              <Meter used={admission.tenants} limit={admission.max_tenants} />
            </div>
          </SettingsRow>
          <SettingsRow className="text-sm"
            title="Provisioning now"
            control={<span className="tabular-nums">{admission.allocating} of {admission.max_concurrent}</span>}
          />
          <SettingsRow className="text-sm"
            title="Free disk"
            description={admission.disk_measured ? `Admission stops below ${formatBytes(admission.min_free_disk_bytes)}.` : "Not measurable on this host."}
            control={<span className="tabular-nums">{admission.disk_measured ? formatBytes(admission.free_disk_bytes) : "—"}</span>}
          />
          <SettingsRow className="text-sm"
            title="Available memory"
            description={
              admission.memory_measured
                ? `Admission stops below ${formatBytes(admission.min_available_memory_bytes)}.`
                : "Not measurable on this host."
            }
            control={
              <span className="tabular-nums">{admission.memory_measured ? formatBytes(admission.available_memory_bytes) : "—"}</span>
            }
          />
        </>
      )}
    </>
  );
}

export function TenantsTable({ value }: { readonly value: PlatformTenants }): React.ReactElement {
  if (value.tenants.length === 0) return <EmptyNote>No tenant Hubs are allocated.</EmptyNote>;
  return (
    <Table aria-label="Tenant Hubs">
      <TableHeader>
        <TableRow>
          <TableHead>Organization</TableHead>
          <TableHead>Hub</TableHead>
          <TableHead>Up since</TableHead>
          <TableHead className="text-right">Restarts</TableHead>
          <TableHead className="text-right">Database</TableHead>
          <TableHead>Last backup</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {value.tenants.map((tenant) => (
          <TableRow key={tenant.organization.id}>
            <TableCell>
              <Link
                to={`/platform/organizations/${encodeURIComponent(tenant.organization.id)}` as never}
                className="font-medium hover:underline"
              >
                {tenant.organization.name}
              </Link>
              <div className="text-muted-foreground">
                <StateLabel state={tenant.state} />
              </div>
            </TableCell>
            <TableCell>
              <StateLabel state={tenant.running ? "ready" : "failed"} label={tenant.running ? "Running" : "Down"} />
            </TableCell>
            <TableCell>
              <Ago at={tenant.started_at} />
            </TableCell>
            <TableCell className={tenant.restarts > 2 ? "text-right tabular-nums text-warning-foreground" : "text-right tabular-nums"}>
              {tenant.restarts}
            </TableCell>
            <TableCell className="text-right tabular-nums">{formatBytes(tenant.database_bytes)}</TableCell>
            <TableCell>
              <Ago at={tenant.last_backup_at} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

export function ProvisioningPage(): React.ReactElement {
  const api = usePlatformApi();
  const health = useResource<PlatformHealth>(() => api.live.health(), [api]);
  const tenants = useResource<PlatformTenants>(() => api.tenants(), [api]);
  return (
    <PlatformPage crumbs={[{ label: "Provisioning & health" }]} proposed>
      <SettingsSection title="Admission and capacity">
        <Loadable resource={health} label="capacity" rows={4}>
          {(value) => <CapacityRows health={value} />}
        </Loadable>
      </SettingsSection>
      <SettingsSection title="Tenant Hubs">
        <Loadable resource={tenants} label="tenant Hubs">
          {(value) => (
            <>
              {value.backups_configured ? null : (
                <SettingsRow className="text-sm"
                  title="Backups are not scheduled"
                  description="A stopped Hub can be exported by hand with detent hub backup. Nothing runs on a schedule and no restore has been rehearsed."
                  control={<StateLabel state="pending" label="Manual only" />}
                />
              )}
              <TenantsTable value={value} />
            </>
          )}
        </Loadable>
      </SettingsSection>
    </PlatformPage>
  );
}
