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
import type { AttentionItem, PlatformOverview } from "../../../contracts/platform.ts";
import { useResource } from "../../account/useResource.ts";
import type { PlatformHealth } from "../../entry/api.ts";
import { SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { CapacityRows } from "./Provisioning.tsx";
import { Ago, EmptyNote, Loadable, PlatformPage, StateLabel, StatStrip, StatusDot } from "../ui.tsx";

const SEVERITY_TONE: Record<AttentionItem["severity"], "error" | "warn" | "idle"> = {
  critical: "error",
  warning: "warn",
  info: "idle",
};

function attentionTarget(item: AttentionItem): string {
  if (item.kind === "payment_failed" || item.kind === "billing_quarantined") return "/platform/billing";
  if (item.kind === "capacity_low") return "/platform/provisioning";
  if (item.organization !== null) return `/platform/organizations/${encodeURIComponent(item.organization.id)}`;
  return "/platform/provisioning";
}

export function AttentionList({ items }: { readonly items: readonly AttentionItem[] }): React.ReactElement {
  if (items.length === 0) return <EmptyNote>Nothing needs attention. Failures, payment problems and capacity warnings appear here.</EmptyNote>;
  return (
    <ul aria-label="Needs attention" className="divide-y divide-border/50">
      {items.map((item) => (
        <li key={item.id}>
          <Link
            to={attentionTarget(item) as never}
            className="flex items-start gap-3 px-3 py-3 outline-hidden hover:bg-accent/40 focus-visible:bg-accent/40 sm:px-4"
          >
            <StatusDot tone={SEVERITY_TONE[item.severity]} pulse={item.severity === "critical"} className="mt-1.5" />
            <span className="min-w-0 flex-1">
              <span className="block text-sm font-medium">{item.title}</span>
              <span className="block text-[13px] text-muted-foreground">{item.detail}</span>
            </span>
            <span className="shrink-0 text-xs text-muted-foreground">
              <Ago at={item.since} />
            </span>
          </Link>
        </li>
      ))}
    </ul>
  );
}

function stateCount(overview: PlatformOverview, state: string): number {
  return overview.organizations.by_state[state] ?? 0;
}

export function OverviewPage(): React.ReactElement {
  const api = usePlatformApi();
  const overview = useResource<PlatformOverview>(() => api.overview(), [api]);
  const health = useResource<PlatformHealth>(() => api.live.health(), [api]);
  const tenants = health.value?.tenants;
  return (
    <PlatformPage crumbs={[{ label: "Overview" }]} proposed>
      <Loadable resource={overview} label="the overview" rows={3}>
        {(value) => (
          <>
            <StatStrip
              items={[
                { label: "Organizations", value: value.organizations.total },
                { label: "Ready", value: stateCount(value, "ready"), tone: "ok" },
                {
                  label: "Failed",
                  value: stateCount(value, "failed"),
                  tone: stateCount(value, "failed") > 0 ? "error" : undefined,
                },
                {
                  label: "Hubs running",
                  value: tenants === undefined ? "—" : `${tenants.running}/${tenants.expected}`,
                  tone: tenants === undefined ? undefined : tenants.running < tenants.expected ? "warn" : "ok",
                },
                { label: "Active users, 7 days", value: value.users.active_7d },
                { label: "Runners connected", value: `${value.runners.connected}/${value.runners.registered}` },
              ]}
            />
            <SettingsSection title={`Needs attention (${value.attention.length})`}>
              <AttentionList items={value.attention} />
            </SettingsSection>
            <SettingsSection
              title={`Signups, last 7 days (${value.signups.last_7d})`}
              headerAction={
                <Link to="/platform/organizations" className="text-xs text-muted-foreground hover:text-foreground">
                  All organizations
                </Link>
              }
            >
              {value.signups.recent.length === 0 ? (
                <EmptyNote>No organizations were created this week.</EmptyNote>
              ) : (
                <Table aria-label="Recent signups">
                  <TableHeader>
                    <TableRow>
                      <TableHead>Organization</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead>Created</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {value.signups.recent.map((signup) => (
                      <TableRow key={signup.id}>
                        <TableCell>
                          <Link
                            to={`/platform/organizations/${encodeURIComponent(signup.id)}` as never}
                            className="font-medium hover:underline"
                          >
                            {signup.name}
                          </Link>
                          <div className="text-muted-foreground">{signup.creator_email}</div>
                        </TableCell>
                        <TableCell>
                          <StateLabel state={signup.state} />
                        </TableCell>
                        <TableCell>
                          <Ago at={signup.created_at} />
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </SettingsSection>
          </>
        )}
      </Loadable>
      <SettingsSection title="Capacity">
        <Loadable resource={health} label="capacity" rows={3}>
          {(reading) => <CapacityRows health={reading} />}
        </Loadable>
      </SettingsSection>
    </PlatformPage>
  );
}
