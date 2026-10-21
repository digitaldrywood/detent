import type { PlatformOrganization, PlatformTenantDetail } from "./api.ts";

export type TenantSort = "name" | "status" | "created";
export type TenantSegment = "all" | "attention" | "complimentary";
export type TenantSearch = {
  q?: string;
  segment?: TenantSegment;
  plan?: string;
  status?: string;
  sort?: TenantSort;
  direction?: "asc" | "desc";
  tenant?: string;
};

export function validateTenantSearch(
  search: Record<string, unknown>,
): TenantSearch {
  return {
    q: typeof search.q === "string" ? search.q : undefined,
    segment:
      search.segment === "attention" || search.segment === "complimentary"
        ? search.segment
        : undefined,
    plan: typeof search.plan === "string" ? search.plan : undefined,
    status: typeof search.status === "string" ? search.status : undefined,
    sort:
      search.sort === "name" ||
      search.sort === "status" ||
      search.sort === "created"
        ? search.sort
        : undefined,
    direction:
      search.direction === "asc" || search.direction === "desc"
        ? search.direction
        : undefined,
    tenant: typeof search.tenant === "string" ? search.tenant : undefined,
  };
}

const statusLabels: Record<string, string> = {
  ready: "Ready",
  requested: "Provisioning",
  allocating: "Provisioning",
  failed: "Failed",
  deleting: "Deleting",
  deleted: "Deleted",
};
const stepLabels: Record<string, string> = {
  admission: "Capacity allocation",
  allocate_tenant: "Capacity allocation",
  provider_organization: "Organization creation",
  owner_membership: "Owner membership",
  tenant_files: "Tenant files",
  tenant_start: "Tenant startup",
  owner_bootstrap: "Owner setup",
  publish: "Publishing",
};
const planLabels: Record<string, string> = {
  free: "Free",
  pilot_free: "Pilot (legacy)",
  comp_team: "Team (legacy)",
  starter: "Starter",
  team: "Team",
  growth: "Growth",
  scale: "Scale",
  business: "Business",
  enterprise: "Enterprise",
};

export function tenantStatus(state: string): string {
  return statusLabels[state] ?? "Unavailable";
}

export function tenantStep(step: string): string {
  return stepLabels[step] ?? "Tenant setup";
}

export function tenantPlan(plan: string | null): string {
  return plan === null ? "Unavailable" : (planLabels[plan] ?? "Custom plan");
}

export type TenantRow = {
  organization: PlatformOrganization;
  onlineRunners: number | null;
  grantExpiry: string | null;
  creditBalance: number | null;
};

export function tenantRow(
  organization: PlatformOrganization,
  detail?: PlatformTenantDetail,
): TenantRow {
  const grants = detail?.entitlements?.grants;
  const expiries = grants?.map((grant) => grant.expires_at);
  return {
    organization,
    onlineRunners:
      detail?.runners == null
        ? organization.runner_count === 0
          ? 0
          : null
        : detail.runners.filter(
            (runner) =>
              runner.health === "online" || runner.health === "outside_hours",
          ).length,
    grantExpiry:
      expiries?.length &&
      expiries.every((expiry): expiry is string => expiry !== null)
        ? (expiries.sort().at(-1) ?? null)
        : null,
    creditBalance:
      detail?.credit_balance_micros == null
        ? null
        : detail.credit_balance_micros / 1_000_000,
  };
}

export function tenantNeedsAttention({
  organization,
  onlineRunners,
  creditBalance,
}: TenantRow): boolean {
  return (
    organization.state === "failed" ||
    ((organization.runner_count ?? 0) > 0 && onlineRunners === 0) ||
    (creditBalance !== null && creditBalance <= 0)
  );
}

export function filterTenants(
  rows: readonly TenantRow[],
  search: TenantSearch,
): TenantRow[] {
  const query = search.q?.trim().toLowerCase() ?? "";
  const sort = search.sort ?? "created";
  const descending =
    (search.direction ?? (sort === "created" ? "desc" : "asc")) === "desc";
  return rows
    .filter(
      ({ organization, ...metadata }) =>
        [organization.name, organization.id, organization.creator_email].some(
          (value) => value.toLowerCase().includes(query),
        ) &&
        (!search.status ||
          search.status === "all" ||
          tenantStatus(organization.state) === search.status) &&
        (!search.plan ||
          search.plan === "all" ||
          (organization.plan ?? "unavailable") === search.plan) &&
        (search.segment === "attention"
          ? tenantNeedsAttention({ organization, ...metadata })
          : search.segment === "complimentary"
            ? (organization.grants ?? 0) > 0
            : true),
    )
    .sort(({ organization: left }, { organization: right }) => {
      const order =
        sort === "name"
          ? left.name.localeCompare(right.name)
          : sort === "status"
            ? tenantStatus(left.state).localeCompare(tenantStatus(right.state))
            : Date.parse(left.created_at) - Date.parse(right.created_at);
      return (descending ? -order : order) || left.id.localeCompare(right.id);
    });
}
