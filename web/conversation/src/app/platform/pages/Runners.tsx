import { Link } from "@tanstack/react-router";
import React from "react";

import { Badge } from "../../../components/ui/badge.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import type { PlatformRunner, PlatformRunners } from "../../../contracts/platform.ts";
import { useResource } from "../../account/useResource.ts";
import { hostIsBehind } from "../../fleet/HostCard.tsx";
import { SettingsRow, SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { Ago, EmptyNote, Loadable, PlatformPage, StateLabel, StatStrip } from "../ui.tsx";

export function runnerSummary(value: PlatformRunners) {
  const healthy = value.runners.filter((runner) => runner.health === "healthy").length;
  const capacity = value.runners.reduce((sum, runner) => sum + runner.host_capacity, 0);
  const used = value.runners.reduce((sum, runner) => sum + runner.host_used, 0);
  const behind = value.runners.filter((runner) => hostIsBehind(runner.version, value.current_version)).length;
  return { total: value.runners.length, healthy, unhealthy: value.runners.length - healthy, capacity, used, behind };
}

export function RunnersTable({
  runners,
  current,
  showOrganization = true,
}: {
  readonly runners: readonly PlatformRunner[];
  readonly current: string;
  readonly showOrganization?: boolean;
}): React.ReactElement {
  if (runners.length === 0) return <EmptyNote>No runners are enrolled.</EmptyNote>;
  return (
    <Table aria-label="Runners">
      <TableHeader>
        <TableRow>
          <TableHead>Runner</TableHead>
          {showOrganization ? <TableHead>Organization</TableHead> : null}
          <TableHead>Health</TableHead>
          <TableHead className="text-right">Slots</TableHead>
          <TableHead>Version</TableHead>
          <TableHead>Last heartbeat</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {runners.map((runner) => (
          <TableRow key={`${runner.organization.id}/${runner.id}`}>
            <TableCell>
              <div className="font-medium">{runner.display_name}</div>
              <div className="text-muted-foreground">
                {runner.hostname}, {runner.os}/{runner.architecture}
              </div>
            </TableCell>
            {showOrganization ? (
              <TableCell>
                <Link
                  to={`/platform/organizations/${encodeURIComponent(runner.organization.id)}/runners` as never}
                  className="hover:underline"
                >
                  {runner.organization.name}
                </Link>
              </TableCell>
            ) : null}
            <TableCell>
              <StateLabel state={runner.health} />
              {runner.state === "enabled" ? null : <div className="text-muted-foreground">{runner.state}</div>}
            </TableCell>
            <TableCell className="text-right tabular-nums">
              {runner.host_used} / {runner.host_capacity}
            </TableCell>
            <TableCell>
              <span className="font-mono text-xs">{runner.version}</span>
              {hostIsBehind(runner.version, current) ? (
                <Badge variant="warning" size="sm" className="ms-1.5">
                  Behind
                </Badge>
              ) : null}
            </TableCell>
            <TableCell>
              <Ago at={runner.last_heartbeat_at} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

export function RunnersPage(): React.ReactElement {
  const api = usePlatformApi();
  const runners = useResource<PlatformRunners>(() => api.runners(), [api]);
  return (
    <PlatformPage crumbs={[{ label: "Runners & capacity" }]} proposed>
      <Loadable resource={runners} label="runners" rows={6}>
        {(value) => {
          const summary = runnerSummary(value);
          return (
            <>
              <StatStrip
                items={[
                  { label: "Runners", value: summary.total },
                  { label: "Healthy", value: summary.healthy, tone: "ok" },
                  { label: "Stale or paused", value: summary.unhealthy, tone: summary.unhealthy > 0 ? "warn" : undefined },
                  { label: "Slots in use", value: `${summary.used}/${summary.capacity}` },
                  { label: "Behind the Hub", value: summary.behind, tone: summary.behind > 0 ? "warn" : undefined },
                  { label: "Hub version", value: <span className="font-mono text-base">{value.current_version}</span> },
                ]}
              />
              {value.unreachable.length === 0 ? null : (
                <SettingsSection title="Not reporting">
                  <SettingsRow className="text-sm"
                    title={`${value.unreachable.length} organization${value.unreachable.length === 1 ? "" : "s"} did not answer`}
                    description={`Their runners are missing from the list below: ${value.unreachable.map((organization) => organization.name).join(", ")}.`}
                    control={<StateLabel state="stale" label="Partial" />}
                  />
                </SettingsSection>
              )}
              <SettingsSection title="Every runner">
                <RunnersTable runners={value.runners} current={value.current_version} />
              </SettingsSection>
            </>
          );
        }}
      </Loadable>
    </PlatformPage>
  );
}
