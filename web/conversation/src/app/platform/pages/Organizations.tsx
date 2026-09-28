import { Link, useNavigate } from "@tanstack/react-router";
import { SearchIcon } from "lucide-react";
import React from "react";

import { Input } from "../../../components/ui/input.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import { NativeSelect } from "../../account/controls.tsx";
import { useResource } from "../../account/useResource.ts";
import type { PlatformOrganization, PlatformOrganizations } from "../../entry/api.ts";
import { SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { Ago, EmptyNote, Loadable, PlatformPage, StateLabel } from "../ui.tsx";

export interface OrganizationFilter {
  readonly query: string;
  readonly state: string;
  readonly plan: string;
}

export const ALL = "all";

export function filterOrganizations(
  organizations: readonly PlatformOrganization[],
  filter: OrganizationFilter,
): readonly PlatformOrganization[] {
  const query = filter.query.trim().toLowerCase();
  return organizations.filter((organization) => {
    if (filter.state !== ALL && organization.state !== filter.state) return false;
    if (filter.plan !== ALL && organization.plan !== filter.plan) return false;
    if (query === "") return true;
    return [organization.name, organization.id, organization.creator_email].some((field) =>
      field.toLowerCase().includes(query),
    );
  });
}

function options(values: readonly (string | null)[], all: string) {
  const distinct = [...new Set(values.filter((value): value is string => value !== null && value !== ""))].toSorted();
  return [{ value: ALL, label: all }, ...distinct.map((value) => ({ value, label: value.replaceAll("_", " ") }))];
}

function count(value: number | null): string {
  return value === null ? "—" : String(value);
}

export function OrganizationsTable({
  listing,
  filter,
}: {
  readonly listing: PlatformOrganizations;
  readonly filter: OrganizationFilter;
}): React.ReactElement {
  const navigate = useNavigate();
  const rows = filterOrganizations(listing.organizations, filter);
  if (listing.organizations.length === 0) {
    return <EmptyNote>No organizations are registered yet. They appear here as soon as someone signs up.</EmptyNote>;
  }
  if (rows.length === 0) return <EmptyNote>No organization matches these filters.</EmptyNote>;
  return (
    <Table aria-label="Organizations">
      <TableHeader>
        <TableRow>
          <TableHead>Organization</TableHead>
          <TableHead>State</TableHead>
          <TableHead>Plan</TableHead>
          <TableHead className="text-right">Members</TableHead>
          <TableHead className="text-right">Runners</TableHead>
          <TableHead>Billing</TableHead>
          <TableHead>Created</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((organization) => {
          const to = `/platform/organizations/${encodeURIComponent(organization.id)}`;
          return (
            <TableRow
              key={organization.id}
              className="cursor-pointer"
              onClick={(event) => {
                if ((event.target as HTMLElement).closest("a") === null) void navigate({ to } as never);
              }}
            >
              <TableCell>
                <Link to={to as never} className="font-medium hover:underline">
                  {organization.name}
                </Link>
                <div className="text-muted-foreground">
                  {organization.creator_email === "" ? "Registered externally" : organization.creator_email}
                </div>
              </TableCell>
              <TableCell>
                <StateLabel state={organization.state} />
                {organization.error_code === "" ? null : (
                  <div className="text-destructive-foreground">{organization.error_code.replaceAll("_", " ")}</div>
                )}
              </TableCell>
              <TableCell>
                {organization.plan ?? "—"}
                {organization.grants !== null && organization.grants > 0 ? (
                  <div className="text-muted-foreground">
                    +{organization.grants} grant{organization.grants === 1 ? "" : "s"}
                  </div>
                ) : null}
              </TableCell>
              <TableCell className="text-right tabular-nums">{count(organization.member_count)}</TableCell>
              <TableCell className="text-right tabular-nums">{count(organization.runner_count)}</TableCell>
              <TableCell>
                {organization.billing.available ? (
                  <StateLabel state={organization.billing.status ?? "unknown"} />
                ) : (
                  <span className="text-muted-foreground">—</span>
                )}
              </TableCell>
              <TableCell>
                <Ago at={organization.created_at} />
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

export function OrganizationsPage(): React.ReactElement {
  const api = usePlatformApi();
  const listing = useResource<PlatformOrganizations>(() => api.live.organizations(), [api]);
  const [filter, setFilter] = React.useState<OrganizationFilter>({ query: "", state: ALL, plan: ALL });
  const value = listing.value;
  const planUnavailable = value?.unavailable.includes("plan") ?? true;
  const unavailable = value?.unavailable ?? [];
  return (
    <PlatformPage crumbs={[{ label: "Organizations" }]}>
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-56 flex-1">
          <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            type="search"
            aria-label="Search organizations"
            placeholder="Search by name, id or creator"
            className="ps-8"
            value={filter.query}
            onChange={(event) => setFilter({ ...filter, query: event.currentTarget.value })}
          />
        </div>
        <NativeSelect
          aria-label="Filter by state"
          className="w-auto flex-1 sm:w-44 sm:flex-none"
          value={filter.state}
          onValueChange={(state) => setFilter({ ...filter, state })}
          options={options(value?.organizations.map((organization) => organization.state) ?? [], "Every state")}
        />
        <NativeSelect
          aria-label="Filter by plan"
          className="w-auto flex-1 sm:w-44 sm:flex-none"
          value={filter.plan}
          disabled={planUnavailable}
          title={planUnavailable ? "The entry does not report plans yet" : undefined}
          onValueChange={(plan) => setFilter({ ...filter, plan })}
          options={options(value?.organizations.map((organization) => organization.plan) ?? [], "Every plan")}
        />
      </div>
      <SettingsSection
        title={value === undefined ? "Organizations" : `Organizations (${filterOrganizations(value.organizations, filter).length} of ${value.organizations.length})`}
        headerAction={
          unavailable.length === 0 ? null : (
            <span className="text-xs text-muted-foreground">Not reported yet: {unavailable.join(", ").replaceAll("_", " ")}</span>
          )
        }
      >
        <Loadable resource={listing} label="organizations" rows={6}>
          {(loaded) => <OrganizationsTable listing={loaded} filter={filter} />}
        </Loadable>
      </SettingsSection>
    </PlatformPage>
  );
}
