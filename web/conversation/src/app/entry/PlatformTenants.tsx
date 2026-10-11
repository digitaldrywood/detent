import React from "react";
import {
  Link,
  useNavigate,
  useParams,
  useRouterState,
} from "@tanstack/react-router";
import { ArrowDown, ArrowUp } from "lucide-react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import {
  Empty,
  EmptyHeader,
  EmptyTitle,
  EmptyDescription,
  EmptyContent,
} from "../../components/ui/empty.tsx";
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
  AlertDialog,
  AlertDialogClose,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogPopup,
  AlertDialogTitle,
} from "../../components/ui/alert-dialog.tsx";
import {
  ToggleGroup,
  ToggleGroupItem,
} from "../../components/ui/toggle-group.tsx";
import { SettingsSection } from "../settings/settingsLayout.tsx";
import { formatCount, formatUsd } from "../usage/usageFormat.ts";
import {
  filterTenants,
  tenantNeedsAttention,
  tenantPlan,
  tenantRow,
  tenantStatus,
  tenantStep,
  type TenantRow,
  type TenantSearch,
  type TenantSort,
  type TenantSegment,
} from "./tenantList.ts";
import { ControlError } from "../account/controls.tsx";
import { useMutation } from "../account/useResource.ts";
import {
  SUPPORT_REASONS,
  type PlatformOrganization,
  type PlatformOrganizations,
  type PlatformTenantDetail,
} from "./api.ts";
import { TenantPlan } from "./ComplimentaryPlans.tsx";
import { Problem, useEntryApi } from "./EntryScreens.tsx";
import {
  redirectPlatformSignIn,
  usePlatformResource,
} from "./usePlatformResource.ts";

const unavailableLabels: Record<string, string> = {
  plan: "plan",
  grants: "grants",
  member_count: "member count",
  runner_count: "runner count",
};
function day(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toISOString().slice(0, 10);
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
  items,
  onChange,
}: {
  readonly label: string;
  readonly value: string;
  readonly items: readonly { label: string; value: string }[];
  readonly onChange: (value: string) => void;
}): React.ReactElement {
  return (
    <Select
      value={value}
      items={items}
      onValueChange={(next) => onChange(next ?? "all")}
    >
      <SelectTrigger
        size="compact"
        aria-label={label}
        className="w-full sm:w-32"
      >
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
  rows,
  search,
  onSearch,
  onOpen,
  loading = false,
  error,
  onRetry,
}: {
  readonly value?: PlatformOrganizations;
  readonly rows: readonly TenantRow[];
  readonly search: TenantSearch;
  readonly onSearch: (search: TenantSearch) => void;
  readonly onOpen: (id: string) => void;
  readonly loading?: boolean;
  readonly error?: string;
  readonly onRetry: () => void;
}): React.ReactElement {
  const tenants = filterTenants(rows, search);
  const sort = search.sort ?? "created";
  const descending =
    (search.direction ?? (sort === "created" ? "desc" : "asc")) === "desc";
  const change = (next: Partial<TenantSearch>) =>
    onSearch({ ...search, ...next, tenant: undefined });
  const sortHead = (field: TenantSort, title: string) => (
    <TableHead
      aria-sort={
        sort === field ? (descending ? "descending" : "ascending") : "none"
      }
    >
      <Button
        variant="ghost"
        size="compact"
        onClick={() =>
          change({
            sort: field,
            direction:
              sort === field
                ? descending
                  ? "asc"
                  : "desc"
                : field === "created"
                  ? "desc"
                  : "asc",
          })
        }
      >
        {title}
        {sort === field ? descending ? <ArrowDown /> : <ArrowUp /> : null}
      </Button>
    </TableHead>
  );
  const segments: { value: TenantSegment; label: string; count: number }[] = [
    { value: "all", label: "All", count: rows.length },
    {
      value: "attention",
      label: "Needs attention",
      count: rows.filter(tenantNeedsAttention).length,
    },
    {
      value: "complimentary",
      label: "Complimentary",
      count: rows.filter((row) => (row.organization.grants ?? 0) > 0).length,
    },
  ];
  const plans = [
    ...new Set(
      rows.map(({ organization }) => organization.plan ?? "unavailable"),
    ),
  ].sort();
  const statuses = [
    ...new Set(
      rows.map(({ organization }) => tenantStatus(organization.state)),
    ),
  ].sort();
  return (
    <div className="flex min-w-0 flex-col gap-5">
      <div className="flex flex-wrap items-center gap-3">
        <Input
          size="compact"
          aria-label="Search tenants"
          placeholder="Search name, ID or owner email"
          value={search.q ?? ""}
          onChange={(event) =>
            change({ q: event.currentTarget.value || undefined })
          }
          className="min-w-0 sm:w-40 sm:flex-1"
        />
        <ToggleGroup
          aria-label="Tenant segment"
          value={[search.segment ?? "all"]}
          onValueChange={(values) => {
            if (values[0])
              change({
                segment:
                  values[0] === "all"
                    ? undefined
                    : (values[0] as TenantSegment),
              });
          }}
        >
          {segments.map((segment) => (
            <ToggleGroupItem key={segment.value} value={segment.value}>
              {segment.label}
              <Badge
                variant={
                  segment.value === "attention" && segment.count > 0
                    ? "warning"
                    : "outline"
                }
              >
                {loading ? "…" : formatCount(segment.count)}
              </Badge>
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
        <div className="flex w-full gap-2 sm:ml-auto sm:w-auto">
          <TenantFilter
            label="Plan"
            value={search.plan ?? "all"}
            items={[
              { label: "All plans", value: "all" },
              ...plans.map((plan) => ({
                label: tenantPlan(plan === "unavailable" ? null : plan),
                value: plan,
              })),
            ]}
            onChange={(plan) =>
              change({ plan: plan === "all" ? undefined : plan })
            }
          />
          <TenantFilter
            label="Status"
            value={search.status ?? "all"}
            items={[
              { label: "All statuses", value: "all" },
              ...statuses.map((status) => ({ label: status, value: status })),
            ]}
            onChange={(status) =>
              change({ status: status === "all" ? undefined : status })
            }
          />
        </div>
      </div>
      {value?.unavailable.length ? (
        <p className="text-xs text-muted-foreground">
          Not shown:{" "}
          {value.unavailable
            .map((field) => unavailableLabels[field] ?? "other tenant fields")
            .join(", ")}
          . The entry could not read these fields.
        </p>
      ) : null}
      {rows.some(
        ({ organization, onlineRunners }) =>
          (organization.runner_count ?? 0) > 0 && onlineRunners === null,
      ) ? (
        <p className="text-xs text-muted-foreground">
          Not shown: runner health. Needs attention may be incomplete.
        </p>
      ) : null}
      <SettingsSection
        title={
          loading
            ? "Loading tenants"
            : `${formatCount(tenants.length)} ${tenants.length === 1 ? "tenant" : "tenants"}`
        }
        aria-label="Tenant list"
        className="min-w-0"
        headerAction={
          <span className="text-xs text-muted-foreground">
            {sort === "created"
              ? descending
                ? "Newest first"
                : "Oldest first"
              : `${sort === "name" ? "Name" : "Status"} · ${descending ? "Z–A" : "A–Z"}`}
          </span>
        }
      >
        {error ? (
          <Empty role="alert">
            <EmptyHeader>
              <EmptyTitle>Tenants could not be loaded</EmptyTitle>
              <EmptyDescription>{error}</EmptyDescription>
            </EmptyHeader>
            <EmptyContent>
              <Button variant="outline" size="compact" onClick={onRetry}>
                Try again
              </Button>
            </EmptyContent>
          </Empty>
        ) : !loading && tenants.length === 0 ? (
          <Empty>
            <EmptyHeader>
              <EmptyTitle>
                {rows.length === 0 ? "No tenants yet" : "No tenants match"}
              </EmptyTitle>
              <EmptyDescription>
                {rows.length === 0
                  ? "Tenants will appear here when they are registered."
                  : "Try another search or change the filters."}
              </EmptyDescription>
            </EmptyHeader>
          </Empty>
        ) : (
          <Table
            aria-label="Tenants"
            aria-busy={loading}
            className="min-w-[880px]"
          >
            <TableHeader>
              <TableRow>
                {sortHead("name", "Tenant")}
                {sortHead("status", "Status")}
                <TableHead>Plan</TableHead>
                <TableHead className="text-right">AI credits</TableHead>
                <TableHead>Members</TableHead>
                <TableHead>Runners</TableHead>
                {sortHead("created", "Created")}
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading
                ? Array.from({ length: 4 }, (_, index) => (
                    <TableRow key={index}>
                      {Array.from({ length: 7 }, (_, cell) => (
                        <TableCell key={cell}>
                          <Skeleton className="h-5 w-20" />
                        </TableCell>
                      ))}
                    </TableRow>
                  ))
                : tenants.map(
                    ({
                      organization: tenant,
                      onlineRunners,
                      grantExpiry,
                      creditBalance,
                    }) => (
                      <TableRow
                        key={tenant.id}
                        className="cursor-pointer"
                        onClick={(event) => {
                          if (!(event.target as HTMLElement).closest("a"))
                            onOpen(tenant.id);
                        }}
                      >
                        <TableCell>
                          <Link
                            to={"/platform/tenants/$tenantId" as never}
                            params={{ tenantId: tenant.id } as never}
                            search={search as never}
                            className="font-medium hover:underline"
                          >
                            {tenant.name}
                          </Link>
                          <div className="text-xs text-muted-foreground">
                            {tenant.creator_email || "Registered externally"}
                          </div>
                        </TableCell>
                        <TableCell>
                          <Badge variant={stateVariant(tenant.state)}>
                            {tenantStatus(tenant.state)}
                          </Badge>
                          {tenant.state === "failed" ? (
                            <div className="mt-1 text-xs text-destructive-foreground">
                              {tenantStep(tenant.step)}
                            </div>
                          ) : null}
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-2">
                            {tenantPlan(tenant.plan)}
                            {(tenant.grants ?? 0) > 0 ? (
                              <Badge variant="info">Complimentary</Badge>
                            ) : null}
                          </div>
                          {(tenant.grants ?? 0) > 0 && grantExpiry ? (
                            <div className="text-xs text-muted-foreground">
                              Until {day(grantExpiry)}
                            </div>
                          ) : null}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {creditBalance === null
                            ? "Unavailable"
                            : formatUsd(creditBalance)}
                        </TableCell>
                        <TableCell>
                          {tenant.member_count === null ? (
                            "Unavailable"
                          ) : (
                            <Badge variant="outline">
                              {formatCount(tenant.member_count)}
                            </Badge>
                          )}
                        </TableCell>
                        <TableCell>
                          {tenant.runner_count === null ||
                          onlineRunners === null ? (
                            "Unavailable"
                          ) : (
                            <Badge
                              variant={
                                onlineRunners > 0
                                  ? "success"
                                  : tenant.runner_count > 0
                                    ? "warning"
                                    : "outline"
                              }
                            >
                              {tenant.runner_count === 0
                                ? "0 runners"
                                : `${formatCount(onlineRunners)} online`}
                            </Badge>
                          )}
                        </TableCell>
                        <TableCell className="tabular-nums">
                          {day(tenant.created_at)}
                        </TableCell>
                      </TableRow>
                    ),
                  )}
            </TableBody>
          </Table>
        )}
      </SettingsSection>
    </div>
  );
}

function SupportAction({
  organization,
  csrf,
}: {
  readonly organization: PlatformOrganization;
  readonly csrf: string;
}): React.ReactElement {
  const reasonId = `support-reason-${organization.id}`;
  return (
    <form
      method="post"
      action="/support/start"
      className="flex flex-wrap items-center gap-2"
    >
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
        className="h-9 rounded-lg border border-input bg-background px-2 text-sm"
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

function ResumeAction({
  detail,
  onChanged,
}: {
  readonly detail: PlatformTenantDetail;
  readonly onChanged: () => Promise<void>;
}): React.ReactElement {
  const api = useEntryApi();
  const [open, setOpen] = React.useState(false);
  const resume = useMutation(() =>
    api.resumePlatformTenant({
      organization: detail.organization.id,
      csrf: detail.csrf,
    }),
  );
  const submit = async () => {
    if ((await resume.call()) !== null) {
      await onChanged();
      setOpen(false);
    }
  };
  React.useEffect(() => {
    redirectPlatformSignIn(resume.error);
  }, [resume.error]);
  return (
    <AlertDialog open={open} onOpenChange={setOpen}>
      <Button
        size="sm"
        variant="outline"
        onClick={() => {
          resume.clearError();
          setOpen(true);
        }}
      >
        Resume provisioning
      </Button>
      <AlertDialogPopup>
        <AlertDialogHeader>
          <AlertDialogTitle>
            Resume provisioning for {detail.organization.name}?
          </AlertDialogTitle>
          <AlertDialogDescription>
            Continue setup from the last completed step. Existing capacity and
            retry rules apply.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <ControlError message={resume.error?.message ?? null} />
        <AlertDialogFooter>
          <AlertDialogClose
            render={
              <Button variant="outline" disabled={resume.pending}>
                Cancel
              </Button>
            }
          />
          <Button disabled={resume.pending} onClick={() => void submit()}>
            {resume.pending ? "Resuming…" : "Resume"}
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  );
}

function TenantSection({
  title,
  children,
}: {
  readonly title: string;
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <section aria-label={title} className="border-t border-border pt-4">
      <h3 className="mb-2 text-sm font-medium">{title}</h3>
      {children}
    </section>
  );
}

export function TenantSections({
  detail,
  role,
  onChanged,
}: {
  readonly detail: PlatformTenantDetail;
  readonly role: string;
  readonly onChanged: () => Promise<void>;
}): React.ReactElement {
  const tenant = detail.organization;
  const canSupport =
    (role === "support" || role === "admin") &&
    tenant.state === "ready" &&
    tenant.can_support;
  const canGrant =
    (role === "billing" || role === "admin") &&
    detail.can_grant &&
    tenant.state === "ready";
  const canResume =
    role === "admin" && tenant.state === "failed" && detail.can_resume;
  const unavailable = (
    <p className="text-sm text-muted-foreground">Unavailable</p>
  );
  return (
    <div className="flex flex-col gap-4 text-sm">
      <div>
        <TenantState organization={tenant} />
      </div>
      {canSupport || canResume ? (
        <TenantSection title="Actions">
          <div className="flex flex-wrap gap-2">
            {canSupport ? (
              <SupportAction organization={tenant} csrf={detail.csrf} />
            ) : null}
            {canResume ? (
              <ResumeAction detail={detail} onChanged={onChanged} />
            ) : null}
          </div>
        </TenantSection>
      ) : null}
      {detail.entitlements === null ? (
        <TenantSection title="Plan">{unavailable}</TenantSection>
      ) : (
        <TenantPlan
          organization={tenant}
          csrf={detail.csrf}
          value={detail.entitlements}
          canGrant={canGrant}
          onChanged={onChanged}
        />
      )}
      <TenantSection title="Billing">
        {tenant.billing.available ? (
          <p>
            {tenant.billing.status}
            {tenant.billing.customer_id
              ? ` · customer ${tenant.billing.customer_id}`
              : ""}
            {tenant.billing.plan ? ` · ${tenant.billing.plan}` : ""}
            {tenant.billing.price_label
              ? ` · ${tenant.billing.price_label}`
              : ""}
          </p>
        ) : (
          unavailable
        )}
      </TenantSection>
      <TenantSection
        title={`Members${detail.members === null ? "" : ` (${detail.members.length})`}`}
      >
        {detail.members === null ? (
          unavailable
        ) : detail.members.length === 0 ? (
          <p className="text-muted-foreground">No members.</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {detail.members.map((member) => (
              <li key={member.id}>
                <Badge variant="outline">{member.role}</Badge> {member.email}
              </li>
            ))}
          </ul>
        )}
      </TenantSection>
      <TenantSection
        title={`Runners${detail.runners === null ? "" : ` (${detail.runners.length})`}`}
      >
        {detail.runners === null ? (
          unavailable
        ) : detail.runners.length === 0 ? (
          <p className="text-muted-foreground">No runners.</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {detail.runners.map((runner) => (
              <li key={runner.id}>
                {runner.name || runner.id}{" "}
                <Badge
                  variant={runner.health === "online" ? "success" : "outline"}
                >
                  {runner.health}
                </Badge>
              </li>
            ))}
          </ul>
        )}
      </TenantSection>
      <TenantSection
        title={`Projects${detail.projects === null ? "" : ` (${detail.projects.length})`}`}
      >
        {detail.projects === null ? (
          unavailable
        ) : detail.projects.length === 0 ? (
          <p className="text-muted-foreground">No projects.</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {detail.projects.map((project) => (
              <li key={project.id}>{project.name}</li>
            ))}
          </ul>
        )}
      </TenantSection>
      <TenantSection title="Provisioning timeline">
        {detail.events.length === 0 ? (
          <p className="text-muted-foreground">No provisioning events.</p>
        ) : (
          <ol className="flex flex-col gap-3">
            {detail.events.map((event, index) => (
              <li key={`${event.recorded_at}-${index}`}>
                <time className="font-mono text-xs text-muted-foreground">
                  {event.recorded_at}
                </time>
                <div>
                  {event.event}{" "}
                  <span className="text-muted-foreground">
                    (generation {event.generation})
                  </span>
                </div>
              </li>
            ))}
          </ol>
        )}
      </TenantSection>
    </div>
  );
}

export function PlatformTenant({
  role,
}: {
  readonly role: string;
}): React.ReactElement {
  const api = useEntryApi();
  const { tenantId } = useParams({ strict: false }) as { tenantId: string };
  const search = useRouterState({
    select: (state) => state.location.search as TenantSearch,
  });
  const resource = usePlatformResource(
    () => api.platformTenant(tenantId),
    [api, tenantId],
  );
  const detail = resource.value;
  return (
    <section
      aria-label={detail?.organization.name ?? "Tenant"}
      className="flex min-w-0 flex-col gap-4"
    >
      <Link
        to={"/platform/tenants" as never}
        search={{ ...search, tenant: undefined } as never}
        className="text-sm text-muted-foreground hover:underline"
      >
        ← Back to tenants
      </Link>
      <div>
        <h2 className="text-lg font-medium">
          {detail?.organization.name ?? "Tenant"}
        </h2>
        <p className="break-words text-xs text-muted-foreground">
          {tenantId}
          {detail ? ` · created ${day(detail.organization.created_at)}` : ""}
        </p>
      </div>
      <Problem
        message={
          resource.error === null
            ? null
            : `This tenant could not be reloaded and may be out of date: ${resource.error.message}`
        }
      />
      {detail === undefined ? (
        resource.loading ? (
          <Skeleton className="h-48" aria-label="Loading tenant" />
        ) : null
      ) : (
        <TenantSections
          detail={detail}
          role={role}
          onChanged={resource.refresh}
        />
      )}
    </section>
  );
}

export function PlatformTenants(): React.ReactElement {
  const api = useEntryApi();
  const resource = usePlatformResource(async () => {
    const value = await api.platformOrganizations();
    const rows = await Promise.all(
      value.organizations.map(async (organization) => {
        if (
          organization.state !== "ready" ||
          ((organization.runner_count ?? 0) === 0 &&
            (organization.grants ?? 0) === 0)
        )
          return tenantRow(organization);
        try {
          return tenantRow(
            organization,
            await api.platformTenant(organization.id),
          );
        } catch (error) {
          redirectPlatformSignIn(error);
          return tenantRow(organization);
        }
      }),
    );
    return { value, rows };
  }, [api]);
  const search = useRouterState({
    select: (state) => state.location.search as TenantSearch,
  });
  const navigate = useNavigate();
  return (
    <TenantTable
      value={resource.value?.value}
      rows={resource.value?.rows ?? []}
      search={search}
      onSearch={(search) => {
        void navigate({
          to: "/platform/tenants",
          search,
          replace: true,
          resetScroll: false,
        } as never);
      }}
      onOpen={(tenantId) => {
        void navigate({
          to: "/platform/tenants/$tenantId",
          params: { tenantId },
          search,
        } as never);
      }}
      loading={resource.loading && resource.value === undefined}
      error={
        resource.error?.status === 403
          ? "The platform console is limited to Detent staff."
          : resource.error
            ? "Try again to reload the tenant list."
            : undefined
      }
      onRetry={() => {
        void resource.refresh();
      }}
    />
  );
}
