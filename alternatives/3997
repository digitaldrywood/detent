import React from "react";
import { Link } from "@tanstack/react-router";
import { RefreshCw } from "lucide-react";

import { Alert, AlertTitle } from "../../components/ui/alert.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Spinner } from "../../components/ui/spinner.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import { UsageMeter, formatUsageValue } from "../../components/ui/usage-meter.tsx";
import { type Resource } from "../account/useResource.ts";
import { SettingsSection, SettingsRow, useRelativeTimeTick } from "../settings/settingsLayout.tsx";
import { type PlatformHealth } from "./api.ts";
import { Problem } from "./EntryScreens.tsx";
import { PlatformPageActions } from "./PlatformPageActions.tsx";

type HealthReading = PlatformHealth & { readonly updatedAt: number };
export const PlatformHealthContext = React.createContext<Resource<HealthReading | null> | null>(null);

export function formatBytes(value: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let amount = value;
  let unit = 0;
  while (amount >= 1024 && unit < units.length - 1) {
    amount /= 1024;
    unit += 1;
  }
  return `${unit === 0 ? amount : amount.toFixed(1)} ${units[unit]}`;
}

function floorState(value: number, floor: number): "normal" | "warning" | "error" {
  return value <= floor ? "error" : value <= floor * 1.2 ? "warning" : "normal";
}

export function platformHealthState(value: PlatformHealth): "success" | "warning" | "error" {
  if (!value.registry.ok || (value.tenants !== undefined && value.tenants.running < value.tenants.expected) ||
    value.tenant_hubs?.some((hub) => !hub.running)) return "error";
  const a = value.admission;
  if (a === undefined) return value.tenants === undefined ? "warning" : "success";
  const floors = [
    a.disk_measured ? floorState(a.free_disk_bytes, a.min_free_disk_bytes) : "warning",
    a.memory_measured ? floorState(a.available_memory_bytes, a.min_available_memory_bytes) : "warning",
    a.max_tenants > 0 ? a.tenants >= a.max_tenants ? "error" : a.tenants >= a.max_tenants * 0.8 ? "warning" : "normal" : "normal",
  ];
  return floors.includes("error") ? "error" : floors.includes("warning") ? "warning" : "success";
}

function FloorRow({ label, measured, current, floor }: {
  readonly label: string;
  readonly measured: boolean;
  readonly current: number;
  readonly floor: number;
}): React.ReactElement {
  const state = floorState(current, floor);
  return <SettingsRow title={label} description={`Floor ${formatBytes(floor)}`} control={
    measured ? <div className="w-full sm:w-64"><UsageMeter label={label} used={current} limit={floor} direction="minimum" unit="bytes"
      valueText={formatBytes(current)} statusText={state === "error" ? "At limit" : state === "warning" ? "Near limit" : "Above floor"} /></div>
      : <span className="text-sm text-muted-foreground">Not measurable</span>
  } />;
}

export function PlatformHealthPanel({ value }: { readonly value: PlatformHealth }): React.ReactElement {
  const a = value.admission;
  const state = platformHealthState(value);
  return <div className="space-y-6">
    <Alert variant={state} aria-label="Service health">
      <Badge variant={state}>{state === "error" ? "Down" : state === "warning" ? "Degraded" : "OK"}</Badge>
      <AlertTitle>{value.registry.ok ? "Registry reachable." : "Registry unreachable."} {value.tenants === undefined ? "Tenant Hub status unavailable." : `${value.tenants.running} of ${value.tenants.expected} tenant Hubs running.`}</AlertTitle>
    </Alert>
    <SettingsSection title="Capacity" aria-label="Capacity" headerAction={<span className="text-right text-xs text-muted-foreground">New tenants are refused below a floor</span>}>
      {a === undefined ? <p className="p-4 text-sm text-muted-foreground">Self-service allocation is not configured, so there are no admission limits.</p> : <>
        <FloorRow label="Free disk" measured={a.disk_measured} current={a.free_disk_bytes} floor={a.min_free_disk_bytes} />
        <FloorRow label="Available memory" measured={a.memory_measured} current={a.available_memory_bytes} floor={a.min_available_memory_bytes} />
        <SettingsRow title="Tenant slots" control={<div className="w-full sm:w-64"><UsageMeter label="Tenant slots" used={a.tenants} limit={a.max_tenants}
          unlimited={a.max_tenants <= 0} valueText={`${a.tenants} / ${a.max_tenants > 0 ? a.max_tenants : "Unlimited"}`}
          statusText={a.max_tenants > 0 && a.tenants >= a.max_tenants ? "At limit" : a.max_tenants > 0 && a.tenants >= a.max_tenants * 0.8 ? "Near limit" : "Within limit"} /></div>} />
        <SettingsRow title="Provisioning now" control={<Badge variant="outline">{a.allocating}</Badge>} />
      </>}
    </SettingsSection>
    <SettingsSection title="Tenant Hubs" aria-label="Tenant Hubs" headerAction={value.tenant_hubs === undefined ? undefined : <Badge variant="outline">{value.tenant_hubs.length}</Badge>}>
      {value.tenant_hubs === undefined ? <p className="p-4 text-sm text-muted-foreground">Tenant Hub details unavailable.</p> : value.tenant_hubs.length === 0 ? <p className="p-4 text-sm text-muted-foreground">No tenant Hubs to monitor.</p> :
        <Table aria-label="Tenant Hubs">
          <TableHeader><TableRow><TableHead>Tenant</TableHead><TableHead>Hub</TableHead><TableHead>Up for</TableHead><TableHead className="text-right">Restarts (24h)</TableHead><TableHead className="text-right">Database size</TableHead></TableRow></TableHeader>
          <TableBody>{value.tenant_hubs.map((hub) => <TableRow key={hub.id}>
            <TableCell><Link to="/platform/tenants" search={{ tenant: hub.id }} className="text-primary underline underline-offset-4">{hub.name}</Link></TableCell>
            <TableCell><Badge variant={hub.running ? "success" : "error"}>{hub.running ? "Running" : "Down"}</Badge></TableCell>
            <TableCell>{!hub.running ? "—" : hub.uptime_seconds === null ? "Unavailable" : formatUsageValue(hub.uptime_seconds, "seconds")}</TableCell>
            <TableCell className="text-right tabular-nums">{hub.restarts_24h ?? "Unavailable"}</TableCell>
            <TableCell className="text-right tabular-nums">{hub.database_bytes === null ? "Unavailable" : formatBytes(hub.database_bytes)}</TableCell>
          </TableRow>)}</TableBody>
        </Table>}
    </SettingsSection>
  </div>;
}

export function PlatformHealthContent(): React.ReactElement {
  const health = React.useContext(PlatformHealthContext)!;
  const now = useRelativeTimeTick();
  const reading = health.value;
  const pending = React.useRef(health.loading);
  pending.current = health.loading;
  React.useEffect(() => {
    const refresh = () => {
      if (document.visibilityState === "visible" && !pending.current) void health.refresh();
    };
    const interval = window.setInterval(refresh, 30_000);
    document.addEventListener("visibilitychange", refresh);
    return () => {
      window.clearInterval(interval);
      document.removeEventListener("visibilitychange", refresh);
    };
  }, [health.refresh]);
  return <>
    <PlatformPageActions>
      {reading ? <span className="text-xs text-muted-foreground">Updated {Math.max(0, Math.floor((now - reading.updatedAt) / 1000))}s ago</span> : null}
      <Button variant="outline" size="sm" disabled={health.loading} aria-busy={health.loading} onClick={() => void health.refresh()}>
        {health.loading ? <Spinner /> : <RefreshCw />}Refresh
      </Button>
    </PlatformPageActions>
    <Problem message={health.error?.message ?? null} />
    {reading ? <PlatformHealthPanel value={reading} /> : health.loading ? <Skeleton className="h-48" aria-label="Loading health" /> : null}
  </>;
}
