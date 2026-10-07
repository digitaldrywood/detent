import React from "react";
import { useNavigate, useRouterState } from "@tanstack/react-router";
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
import {
  Sheet,
  SheetDescription,
  SheetHeader,
  SheetPanel,
  SheetPopup,
  SheetTitle,
} from "../../components/ui/sheet.tsx";
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
import {
  AlertDialog,
  AlertDialogClose,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogPopup,
  AlertDialogTitle,
} from "../../components/ui/alert-dialog.tsx";
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

function TenantDetail({
  id,
  role,
  onChanged,
}: {
  readonly id: string;
  readonly role: string;
  readonly onChanged: () => Promise<void>;
}): React.ReactElement {
  const api = useEntryApi();
  const resource = usePlatformResource(() => api.platformTenant(id), [api, id]);
  const refresh = React.useCallback(async () => {
    await resource.refresh();
    await onChanged();
  }, [resource.refresh, onChanged]);
  const detail = resource.value;
  return (
    <>
      <SheetHeader>
        <SheetTitle>{detail?.organization.name ?? "Tenant"}</SheetTitle>
        <SheetDescription className="break-words">
          {detail === undefined
            ? id
            : `${id} · created ${day(detail.organization.created_at)}${detail.organization.creator_email ? ` by ${detail.organization.creator_email}` : " · Registered externally"}`}
        </SheetDescription>
      </SheetHeader>
      <SheetPanel>
        <Problem
          message={
            resource.error === null
              ? null
              : `This tenant could not be reloaded and may be out of date: ${resource.error.message}`
          }
        />
        {detail === undefined ? (
          resource.loading ? (
            <div className="flex flex-col gap-4" aria-label="Loading tenant">
              <Skeleton className="h-20" />
              <Skeleton className="h-40" />
            </div>
          ) : null
        ) : (
          <TenantSections detail={detail} role={role} onChanged={refresh} />
        )}
      </SheetPanel>
    </>
  );
}

export function PlatformTenants({
  role,
}: {
  readonly role: string;
}): React.ReactElement {
  const api = useEntryApi();
  const resource = usePlatformResource(
    () => api.platformOrganizations(),
    [api],
  );
  const search = useRouterState({
    select: (state) => state.location.search as { tenant?: string },
  });
  const navigate = useNavigate();
  const selectTenant = (tenant?: string) => {
    void navigate({ to: "/platform/tenants", search: { tenant } } as never);
  };
  return (
    <>
      <Problem
        message={
          resource.error?.status === 403
            ? "The platform console is limited to Detent staff."
            : (resource.error?.message ?? null)
        }
      />
      {resource.value === undefined ? (
        resource.loading ? (
          <Skeleton className="h-48" aria-label="Loading tenants" />
        ) : null
      ) : (
        <TenantTable value={resource.value} onOpen={selectTenant} />
      )}
      <Sheet
        open={search.tenant !== undefined}
        onOpenChange={(open) => {
          if (!open) selectTenant();
        }}
      >
        <SheetPopup className="w-full sm:max-w-xl">
          {search.tenant === undefined ? null : (
            <TenantDetail
              key={search.tenant}
              id={search.tenant}
              role={role}
              onChanged={resource.refresh}
            />
          )}
        </SheetPopup>
      </Sheet>
    </>
  );
}
