// `/platform` on the shared entry: the Detent staff console. It lists every
// registered organization with its provisioning state, shows the signup
// allowlist and where it is configured, and reports service health from the
// entry's own admission readings. Support access starts through the existing
// `/support/start` form, so the entry re-checks the actor, CSRF and reason.
import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table.tsx";
import { DetentCloudLogo } from "../account/Login.tsx";
import { useResource } from "../account/useResource.ts";
import {
  type PlatformAllowlist,
  type PlatformHealth,
  type PlatformOrganization,
  type PlatformOrganizations,
  SIGN_IN_PLATFORM,
  SUPPORT_REASONS,
} from "./api.ts";
import { Problem, SignOut, useEntryApi } from "./EntryScreens.tsx";

const UNAVAILABLE_LABELS: Record<string, string> = {
  plan: "plan",
  grants: "grants",
  member_count: "member count",
  runner_count: "runner count",
};

function Panel({
  title,
  description,
  children,
}: {
  readonly title: string;
  readonly description?: string;
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <section aria-label={title} className="rounded-2xl border border-border bg-card p-5">
      <h2 className="text-base font-semibold">{title}</h2>
      {description === undefined ? null : (
        <p className="mt-1 text-[13px] text-muted-foreground">{description}</p>
      )}
      <div className="mt-4">{children}</div>
    </section>
  );
}

function stateVariant(state: string): "success" | "error" | "warning" | "outline" {
  if (state === "ready") return "success";
  if (state === "failed") return "error";
  if (state === "requested" || state === "allocating" || state === "deleting") return "warning";
  return "outline";
}

function formatDate(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toISOString().slice(0, 10);
}

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

function SupportAction({
  organization,
  csrf,
}: {
  readonly organization: PlatformOrganization;
  readonly csrf: string;
}): React.ReactElement {
  if (!organization.can_support) {
    return (
      <span className="text-muted-foreground">
        {organization.state === "ready" ? "Support actors only" : "Not ready"}
      </span>
    );
  }
  const reasonId = `support-reason-${organization.id}`;
  return (
    <form method="post" action="/support/start" className="flex flex-wrap items-center gap-2">
      <input type="hidden" name="organization" value={organization.id} />
      <input type="hidden" name="csrf" value={csrf} />
      <label htmlFor={reasonId} className="sr-only">
        Support reason for {organization.name}
      </label>
      <select
        id={reasonId}
        name="reason"
        required
        defaultValue=""
        className="h-8 rounded-md border border-input bg-background px-2 text-xs"
      >
        <option value="" disabled>
          Reason…
        </option>
        {SUPPORT_REASONS.map((reason) => (
          <option key={reason} value={reason}>
            {reason}
          </option>
        ))}
      </select>
      <Button type="submit" size="sm" variant="outline">
        Start support access
      </Button>
    </form>
  );
}

export function PlatformOrganizationsPanel({
  value,
}: {
  readonly value: PlatformOrganizations;
}): React.ReactElement {
  const unavailable = value.unavailable.map((field) => UNAVAILABLE_LABELS[field] ?? field);
  return (
    <Panel
      title="Organizations"
      description={
        unavailable.length === 0
          ? undefined
          : `Not shown: ${unavailable.join(", ")}. The entry cannot read them without a new tenant query.`
      }
    >
      {value.organizations.length === 0 ? (
        <p className="text-sm text-muted-foreground">No organizations are registered.</p>
      ) : (
        <Table aria-label="Organizations">
          <TableHeader>
            <TableRow>
              <TableHead>Organization</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Owner</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Billing</TableHead>
              <TableHead>Support access</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {value.organizations.map((organization) => (
              <TableRow key={organization.id}>
                <TableCell>
                  <div className="font-medium">{organization.name}</div>
                  <div className="font-mono text-muted-foreground">{organization.id}</div>
                </TableCell>
                <TableCell>
                  <Badge variant={stateVariant(organization.state)}>{organization.state}</Badge>
                  {organization.error_code === "" ? null : (
                    <div className="mt-1 text-destructive-foreground">
                      Last error: {organization.error_code}
                      {organization.step === "" ? "" : ` after ${organization.step}`}
                    </div>
                  )}
                </TableCell>
                <TableCell>
                  {organization.owner_email === "" ? (
                    <span className="text-muted-foreground">Registered externally</span>
                  ) : (
                    organization.owner_email
                  )}
                </TableCell>
                <TableCell>{formatDate(organization.created_at)}</TableCell>
                <TableCell>
                  {organization.billing.available ? (
                    organization.billing.status
                  ) : (
                    <span className="text-muted-foreground">Unavailable</span>
                  )}
                </TableCell>
                <TableCell>
                  <SupportAction organization={organization} csrf={value.csrf} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Panel>
  );
}

export function PlatformAllowlistPanel({ value }: { readonly value: PlatformAllowlist }): React.ReactElement {
  const source = value.source.file === "" ? "the shared entry configuration" : value.source.file;
  return (
    <Panel
      title="Signup allowlist"
      description={`Read-only. Configured in ${source} under ${value.source.keys.join(" and ")}.`}
    >
      {!value.self_service ? (
        <p className="text-sm text-muted-foreground">Self-service organization creation is not configured.</p>
      ) : value.open === true ? (
        <p className="text-sm">Open: any verified account may create an organization.</p>
      ) : (
        <dl className="grid gap-3 text-sm sm:grid-cols-2">
          <div>
            <dt className="font-medium">Allowed emails</dt>
            <dd className="text-muted-foreground">
              {value.allowed_emails.length === 0 ? "None" : value.allowed_emails.join(", ")}
            </dd>
          </div>
          <div>
            <dt className="font-medium">Allowed domains</dt>
            <dd className="text-muted-foreground">
              {value.allowed_domains.length === 0 ? "None" : value.allowed_domains.join(", ")}
            </dd>
          </div>
        </dl>
      )}
    </Panel>
  );
}

function Reading({ label, value }: { readonly label: string; readonly value: string }): React.ReactElement {
  return (
    <div>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="font-medium">{value}</dd>
    </div>
  );
}

export function PlatformHealthPanel({ value }: { readonly value: PlatformHealth }): React.ReactElement {
  const admission = value.admission;
  return (
    <Panel title="Service health">
      <dl className="grid gap-3 text-sm sm:grid-cols-2">
        <Reading label="Registry" value={value.registry.ok ? "OK" : "Unavailable"} />
        {value.tenants === undefined ? null : (
          <Reading label="Tenant Hubs running" value={`${value.tenants.running} of ${value.tenants.expected}`} />
        )}
        {admission === undefined ? null : (
          <>
            <Reading label="Tenant slots" value={`${admission.tenants} of ${admission.max_tenants}`} />
            <Reading label="Provisioning now" value={`${admission.allocating} of ${admission.max_concurrent}`} />
            <Reading
              label="Free disk"
              value={
                admission.disk_measured
                  ? `${formatBytes(admission.free_disk_bytes)} (floor ${formatBytes(admission.min_free_disk_bytes)})`
                  : "Not measurable"
              }
            />
            <Reading
              label="Available memory"
              value={
                admission.memory_measured
                  ? `${formatBytes(admission.available_memory_bytes)} (floor ${formatBytes(admission.min_available_memory_bytes)})`
                  : "Not measurable"
              }
            />
          </>
        )}
      </dl>
      {admission === undefined ? (
        <p className="mt-3 text-[13px] text-muted-foreground">Self-service allocation is not configured, so there are no admission limits.</p>
      ) : null}
    </Panel>
  );
}

export function PlatformConsole(): React.ReactElement {
  const api = useEntryApi();
  const organizations = useResource<PlatformOrganizations>(() => api.platformOrganizations(), [api]);
  const allowlist = useResource<PlatformAllowlist>(() => api.platformAllowlist(), [api]);
  const health = useResource<PlatformHealth>(() => api.platformHealth(), [api]);
  const unauthenticated = [organizations.error, allowlist.error, health.error].some((error) => error?.status === 401);
  React.useEffect(() => {
    if (unauthenticated) globalThis.location?.assign(SIGN_IN_PLATFORM);
  }, [unauthenticated]);
  const value = organizations.value;
  return (
    <div className="flex-1 overflow-y-auto">
      <header className="flex flex-wrap items-center gap-3 border-b border-border/60 px-4 py-3 sm:px-8">
        <DetentCloudLogo />
        <Badge variant="info">Platform</Badge>
        <span className="flex-1" />
        {value === undefined ? null : (
          <>
            <span className="text-sm text-muted-foreground">{value.email}</span>
            <SignOut csrf={value.csrf} />
          </>
        )}
      </header>
      <main className="mx-auto flex w-full max-w-6xl flex-col gap-5 px-4 py-6 sm:px-8">
        <h1 className="text-2xl font-semibold tracking-[-0.02em]">Platform console</h1>
        {organizations.error?.status === 403 ? (
          <Problem message="The platform console is limited to Detent staff." />
        ) : (
          <>
            <Problem message={organizations.error?.message ?? null} />
            {value === undefined ? (
              <p className="text-sm text-muted-foreground">Loading organizations…</p>
            ) : (
              <PlatformOrganizationsPanel value={value} />
            )}
            <div className="grid gap-5 lg:grid-cols-2">
              {allowlist.value === undefined ? (
                <Problem message={allowlist.error?.message ?? null} />
              ) : (
                <PlatformAllowlistPanel value={allowlist.value} />
              )}
              {health.value === undefined ? (
                <Problem message={health.error?.message ?? null} />
              ) : (
                <PlatformHealthPanel value={health.value} />
              )}
            </div>
          </>
        )}
      </main>
    </div>
  );
}
