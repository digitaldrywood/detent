import React from "react";
import { useNavigate } from "@tanstack/react-router";
import { ArrowDown, ArrowUp } from "lucide-react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Empty, EmptyHeader, EmptyTitle } from "../../components/ui/empty.tsx";
import { Input } from "../../components/ui/input.tsx";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table.tsx";
import {
  Tooltip,
  TooltipPopup,
  TooltipProvider,
  TooltipTrigger,
} from "../../components/ui/tooltip.tsx";
import { Problem, useEntryApi } from "./EntryScreens.tsx";
import type { PlatformOrganization, PlatformOrganizations } from "./api.ts";
import { usePlatformResource } from "./usePlatformResource.ts";

const unavailableLabels: Record<string, string> = {
  plan: "plan",
  grants: "grants",
  member_count: "member count",
  runner_count: "runner count",
};
export type TenantSort = "name" | "state" | "created_at";

function filterTenants(
  organizations: readonly PlatformOrganization[],
  search: string,
  state: string,
  plan: string,
  sort: TenantSort,
  descending = false,
): PlatformOrganization[] {
  const query = search.trim().toLowerCase();
  return organizations
    .filter(
      (organization) =>
        [organization.name, organization.id, organization.creator_email].some(
          (value) => value.toLowerCase().includes(query),
        ) &&
        (state === "all" || organization.state === state) &&
        (plan === "all" || (organization.plan ?? "unavailable") === plan),
    )
    .sort((left, right) => {
      const order =
        left[sort].localeCompare(right[sort]) ||
        left.id.localeCompare(right.id);
      return descending ? -order : order;
    });
}

function day(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "Unavailable" : date.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

function stateVariant(
  state: string,
): "success" | "error" | "warning" | "outline" {
  if (state === "ready") return "success";
  if (state === "failed") return "error";
  if (["requested", "allocating", "deleting"].includes(state)) return "warning";
  return "outline";
}

function TenantState({
  organization,
}: {
  readonly organization: PlatformOrganization;
}): React.ReactElement {
  return (
    <>
      <Badge variant={stateVariant(organization.state)}>
        {organization.state}
      </Badge>
      {organization.error_code === "" ? null : (
        <p className="mt-1 whitespace-normal text-xs text-destructive-foreground">
          Last error: {organization.error_code}
          {organization.step === "" ? "" : ` after ${organization.step}`}
          {organization.error_detail ? ` (${organization.error_detail})` : ""}
        </p>
      )}
    </>
  );
}

function TenantFilter({
  label,
  value,
  values,
  onChange,
}: {
  readonly label: string;
  readonly value: string;
  readonly values: readonly string[];
  readonly onChange: (value: string) => void;
}): React.ReactElement {
  const items = [
    { label: `All ${label.toLowerCase()}s`, value: "all" },
    ...values.map((value) => ({
      label: value === "unavailable" ? "Unavailable" : value,
      value,
    })),
  ];
  return (
    <Select
      value={value}
      items={items}
      onValueChange={(next) => onChange(next ?? "all")}
    >
      <SelectTrigger aria-label={label} className="w-full sm:w-40">
        <SelectValue />
      </SelectTrigger>
      <SelectPopup>
        {items.map((item) => (
          <SelectItem key={item.value} value={item.value}>
            {item.label}
          </SelectItem>
        ))}
      </SelectPopup>
    </Select>
  );
}

export function TenantTable({
  value,
  onOpen,
}: {
  readonly value: PlatformOrganizations;
  readonly onOpen: (id: string) => void;
}): React.ReactElement {
  const [search, setSearch] = React.useState("");
  const [state, setState] = React.useState("all");
  const [plan, setPlan] = React.useState("all");
  const [sort, setSort] = React.useState<TenantSort>("created_at");
  const [descending, setDescending] = React.useState(false);
  const tenants = filterTenants(
    value.organizations,
    search,
    state,
    plan,
    sort,
    descending,
  );
  const sortBy = (next: TenantSort) => {
    setSort(next);
    setDescending(sort === next ? !descending : false);
  };
  const sortHead = (field: TenantSort, title: string) => (
    <TableHead
      aria-sort={
        sort === field ? (descending ? "descending" : "ascending") : "none"
      }
    >
      <Button variant="ghost" size="compact" onClick={() => sortBy(field)}>
        {title}
        {sort === field ? descending ? <ArrowDown /> : <ArrowUp /> : null}
      </Button>
    </TableHead>
  );
  return (
    <section aria-label="Tenants" className="flex flex-col gap-4">
      <p
        className="text-sm tabular-nums text-muted-foreground"
        aria-live="polite"
      >
        {tenants.length} tenants ·{" "}
        {tenants.filter((tenant) => tenant.state === "ready").length} ready
      </p>
      {value.unavailable.length === 0 ? null : (
        <p className="text-xs text-muted-foreground">
          Not shown:{" "}
          {value.unavailable
            .map((field) => unavailableLabels[field] ?? field)
            .join(", ")}
          . The entry could not read these fields.
        </p>
      )}
      <div className="flex flex-col gap-3 sm:flex-row">
        <Input
          aria-label="Search tenants"
          placeholder="Search name, id or creator"
          value={search}
          onChange={(event) => setSearch(event.currentTarget.value)}
          className="sm:max-w-sm"
        />
        <TenantFilter
          label="State"
          value={state}
          values={[
            ...new Set(value.organizations.map((tenant) => tenant.state)),
          ].sort()}
          onChange={setState}
        />
        <TenantFilter
          label="Plan"
          value={plan}
          values={[
            ...new Set(
              value.organizations.map((tenant) => tenant.plan ?? "unavailable"),
            ),
          ].sort()}
          onChange={setPlan}
        />
      </div>
      {tenants.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>No tenants match</EmptyTitle>
          </EmptyHeader>
        </Empty>
      ) : (
        <TooltipProvider>
          <Table aria-label="Tenants">
            <TableHeader>
              <TableRow>
                {sortHead("name", "Tenant")}
                {sortHead("state", "State")}
                <TableHead>Plan</TableHead>
                <TableHead>Members</TableHead>
                <TableHead>Runners</TableHead>
                <TableHead>Billing</TableHead>
                {sortHead("created_at", "Created")}
              </TableRow>
            </TableHeader>
            <TableBody>
              {tenants.map((tenant) => (
                <TableRow
                  key={tenant.id}
                  className="cursor-pointer"
                  onClick={() => onOpen(tenant.id)}
                >
                  <TableCell>
                    <Button
                      variant="link"
                      className="h-auto p-0"
                      onClick={(event) => {
                        event.stopPropagation();
                        onOpen(tenant.id);
                      }}
                    >
                      {tenant.name}
                    </Button>
                    <div className="font-mono text-xs text-muted-foreground">
                      {tenant.id}
                    </div>
                  </TableCell>
                  <TableCell>
                    <TenantState organization={tenant} />
                  </TableCell>
                  <TableCell>
                    {tenant.plan ?? "—"}
                    {(tenant.grants ?? 0) > 0 ? (
                      <Tooltip>
                        <TooltipTrigger
                          render={
                            <span
                              tabIndex={0}
                              aria-label="Complimentary grant active"
                            />
                          }
                          className="ml-1"
                        >
                          ✦
                        </TooltipTrigger>
                        <TooltipPopup>Complimentary grant active</TooltipPopup>
                      </Tooltip>
                    ) : null}
                  </TableCell>
                  <TableCell className="tabular-nums">
                    {tenant.member_count ?? "—"}
                  </TableCell>
                  <TableCell className="tabular-nums">
                    {tenant.runner_count ?? "—"}
                  </TableCell>
                  <TableCell>
                    {tenant.billing.available
                      ? tenant.billing.status
                      : "Unavailable"}
                  </TableCell>
                  <TableCell className="tabular-nums">
                    {day(tenant.created_at)}
                    <div className="text-xs text-muted-foreground">
                      {tenant.creator_email || "Registered externally"}
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TooltipProvider>
      )}
    </section>
  );
}

export function PlatformTenants({ role: _role }: { readonly role: string }): React.ReactElement {
  const api = useEntryApi();
  const resource = usePlatformResource(() => api.platformOrganizations(), [api]);
  const navigate = useNavigate();
  return <>
    <Problem message={resource.error?.status === 403 ? "The platform console is limited to Detent staff." : resource.error?.message ?? null} />
    {resource.value === undefined ? resource.loading ? <Skeleton className="h-48" aria-label="Loading tenants" /> : null :
      <TenantTable value={resource.value} onOpen={(organization) => void navigate({ to: "/platform/tenants/$organization", params: { organization } } as never)} />}
  </>;
}
