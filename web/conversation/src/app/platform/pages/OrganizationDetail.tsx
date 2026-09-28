import { useNavigate } from "@tanstack/react-router";
import React from "react";

import { Badge } from "../../../components/ui/badge.tsx";
import { Button } from "../../../components/ui/button.tsx";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "../../../components/ui/dialog.tsx";
import { Label } from "../../../components/ui/label.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import { Toggle, ToggleGroup } from "../../../components/ui/toggle-group.tsx";
import type {
  AuditPage,
  OrganizationBilling,
  OrganizationDetail,
  OrganizationMembers,
  OrganizationProjects,
  OrganizationUsage,
  PlatformRunners,
  ProvisioningHistory,
} from "../../../contracts/platform.ts";
import { NativeSelect } from "../../account/controls.tsx";
import { useResource } from "../../account/useResource.ts";
import { OrganizationPlan } from "../../entry/ComplimentaryPlans.tsx";
import { SUPPORT_REASONS, type PlatformOrganization, type PlatformOrganizations } from "../../entry/api.ts";
import { formatBytes } from "../../entry/PlatformConsole.tsx";
import { allowanceLabel } from "../../settings/Settings.tsx";
import { SettingsRow, SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformAccess } from "../access.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { Ago, EmptyNote, FailedState, Loadable, LoadingRows, PlatformPage, ReasonDialog, StateLabel, day } from "../ui.tsx";
import { AuditTable } from "./Audit.tsx";
import { RunnersTable } from "./Runners.tsx";

export const ORGANIZATION_TABS = [
  { id: "overview", label: "Overview" },
  { id: "members", label: "Members" },
  { id: "projects", label: "Projects" },
  { id: "runners", label: "Runners" },
  { id: "usage", label: "Usage" },
  { id: "plan", label: "Plan" },
  { id: "billing", label: "Billing" },
  { id: "provisioning", label: "Provisioning" },
  { id: "audit", label: "Audit" },
] as const;

export type OrganizationTab = (typeof ORGANIZATION_TABS)[number]["id"];

export function organizationTab(value: string | undefined): OrganizationTab {
  return ORGANIZATION_TABS.find((tab) => tab.id === value)?.id ?? "overview";
}

/**
 * Starts support access through the entry's existing form post, so the entry
 * re-checks the actor, the CSRF token and the reason exactly as it does today.
 */
export function StartSupportDialog({
  organization,
  csrf,
  allowed,
}: {
  readonly organization: Pick<PlatformOrganization, "id" | "name" | "state">;
  readonly csrf: string;
  readonly allowed: boolean;
}): React.ReactElement {
  const [reason, setReason] = React.useState<string>(SUPPORT_REASONS[0]);
  const ready = organization.state === "ready";
  const blocked = !allowed ? "Only support actors can start support access." : !ready ? "The organization is not ready." : null;
  const [open, setOpen] = React.useState(false);
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <Button
        size="sm"
        variant="outline"
        disabled={blocked !== null}
        title={blocked ?? undefined}
        onClick={() => setOpen(true)}
      >
        Start support access
      </Button>
      {blocked === null ? (
        <DialogPopup>
          <form className="flex min-h-0 flex-1 flex-col" method="post" action="/support/start">
            <input type="hidden" name="organization" value={organization.id} />
            <input type="hidden" name="csrf" value={csrf} />
            <DialogHeader>
              <DialogTitle>Start support access to {organization.name}?</DialogTitle>
              <DialogDescription>
                You sign in as a member through WorkOS impersonation for at most one hour. The organization's audit log records
                you, the reason and every request.
              </DialogDescription>
            </DialogHeader>
            <DialogPanel>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={`support-${organization.id}`}>Reason</Label>
                <NativeSelect
                  id={`support-${organization.id}`}
                  name="reason"
                  value={reason}
                  onValueChange={setReason}
                  options={SUPPORT_REASONS.map((value) => ({ value, label: value.replaceAll("-", " ") }))}
                />
              </div>
            </DialogPanel>
            <DialogFooter>
              <DialogClose render={<Button variant="outline">Cancel</Button>} />
              <Button type="submit">Start support access</Button>
            </DialogFooter>
          </form>
        </DialogPopup>
      ) : null}
    </Dialog>
  );
}

function OverviewTab({ detail }: { readonly detail: OrganizationDetail }): React.ReactElement {
  const metadata = detail.metadata;
  return (
    <>
      <SettingsSection title="Registry">
        <SettingsRow className="text-sm" title="Organization id" control={<span className="font-mono text-xs">{detail.id}</span>} />
        <SettingsRow className="text-sm" title="WorkOS organization" control={<span className="font-mono text-xs">{detail.provider_id || "—"}</span>} />
        <SettingsRow className="text-sm"
          title="Created"
          description={detail.creator_email === "" ? "Registered by the operator" : `By ${detail.creator_email}`}
          control={day(detail.created_at)}
        />
        <SettingsRow className="text-sm" title="Managed by the entry" control={detail.managed ? "Yes" : "No, registered externally"} />
        {detail.error_code === "" ? null : (
          <SettingsRow className="text-sm"
            title="Last failure"
            description={`After ${detail.attempts} attempt${detail.attempts === 1 ? "" : "s"} at ${detail.step || "the first step"}.`}
            control={<StateLabel state="failed" label={detail.error_code.replaceAll("_", " ")} />}
          />
        )}
      </SettingsSection>
      <SettingsSection title="Tenant Hub">
        {metadata === null ? (
          <EmptyNote>The Hub did not answer, so its metadata is unavailable.</EmptyNote>
        ) : (
          <>
            <SettingsRow className="text-sm" title="Health" control={<StateLabel state={metadata.healthy ? "ready" : "failed"} label={metadata.healthy ? "Healthy" : "Unhealthy"} />} />
            <SettingsRow className="text-sm" title="Last activity" control={<Ago at={metadata.last_activity} />} />
            <SettingsRow className="text-sm"
              title="Contents"
              control={
                <span className="tabular-nums">
                  {metadata.members} members, {metadata.projects} projects, {metadata.runners} runners
                </span>
              }
            />
            <SettingsRow className="text-sm" title="Recorded events" control={<span className="tabular-nums">{metadata.events.toLocaleString("en-US")}</span>} />
            <SettingsRow className="text-sm" title="Database size" control={<span className="tabular-nums">{formatBytes(metadata.database_bytes)}</span>} />
          </>
        )}
      </SettingsSection>
    </>
  );
}

function MembersTab({ id }: { readonly id: string }): React.ReactElement {
  const api = usePlatformApi();
  const members = useResource<OrganizationMembers>(() => api.members(id), [api, id]);
  return (
    <SettingsSection title="Members">
      <Loadable resource={members} label="members">
        {(value) =>
          value.members.length === 0 ? (
            <EmptyNote>This organization has no members yet.</EmptyNote>
          ) : (
            <>
              <Table aria-label="Members">
                <TableHeader>
                  <TableRow>
                    <TableHead>Member</TableHead>
                    <TableHead>Role</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Last seen</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {value.members.map((member) => (
                    <TableRow key={member.id}>
                      <TableCell className="font-medium">{member.email}</TableCell>
                      <TableCell className="capitalize">{member.role}</TableCell>
                      <TableCell>
                        <StateLabel state={member.status} />
                      </TableCell>
                      <TableCell>
                        <Ago at={member.last_seen_at} />
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              {value.pending_invitations === 0 ? null : (
                <p className="px-4 py-3 text-xs text-muted-foreground">
                  {value.pending_invitations} invitation{value.pending_invitations === 1 ? "" : "s"} pending.
                </p>
              )}
            </>
          )
        }
      </Loadable>
    </SettingsSection>
  );
}

function ProjectsTab({ id }: { readonly id: string }): React.ReactElement {
  const api = usePlatformApi();
  const projects = useResource<OrganizationProjects>(() => api.projects(id), [api, id]);
  return (
    <SettingsSection title="Projects">
      <Loadable resource={projects} label="projects">
        {(value) =>
          value.projects.length === 0 ? (
            <EmptyNote>No projects are set up.</EmptyNote>
          ) : (
            <Table aria-label="Projects">
              <TableHeader>
                <TableRow>
                  <TableHead>Project</TableHead>
                  <TableHead className="text-right">Repositories</TableHead>
                  <TableHead className="text-right">Runners</TableHead>
                  <TableHead>Last activity</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {value.projects.map((project) => (
                  <TableRow key={project.id}>
                    <TableCell>
                      <div className="font-medium">{project.name}</div>
                      <div className="font-mono text-xs text-muted-foreground">{project.id}</div>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{project.repositories}</TableCell>
                    <TableCell className="text-right tabular-nums">{project.runners}</TableCell>
                    <TableCell>
                      <Ago at={project.last_activity} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )
        }
      </Loadable>
    </SettingsSection>
  );
}

function RunnersTab({ id }: { readonly id: string }): React.ReactElement {
  const api = usePlatformApi();
  const runners = useResource<PlatformRunners>(() => api.runners(id), [api, id]);
  return (
    <SettingsSection title="Runners">
      <Loadable resource={runners} label="runners">
        {(value) => <RunnersTable runners={value.runners} current={value.current_version} showOrganization={false} />}
      </Loadable>
    </SettingsSection>
  );
}

function UsageTab({ id }: { readonly id: string }): React.ReactElement {
  const api = usePlatformApi();
  const usage = useResource<OrganizationUsage>(() => api.usage(id), [api, id]);
  return (
    <SettingsSection title="Usage against allowances">
      <Loadable resource={usage} label="usage">
        {(value) =>
          value.allowances.length === 0 ? (
            <EmptyNote>The plan sets no allowances.</EmptyNote>
          ) : (
            <>
              {value.allowances.map((allowance) => {
                const over = allowance.used > allowance.limit;
                const ratio = allowance.limit <= 0 ? 1 : Math.min(allowance.used / allowance.limit, 1);
                return (
                  <SettingsRow className="text-sm"
                    key={allowance.name}
                    title={<span className="capitalize">{allowanceLabel(allowance.name)}</span>}
                    status={over ? <span className="text-destructive-foreground">Over the allowance</span> : undefined}
                    control={
                      <span className="tabular-nums">
                        {allowance.used.toLocaleString("en-US")} of {allowance.limit.toLocaleString("en-US")}
                      </span>
                    }
                  >
                    <div className="pb-3" aria-hidden="true">
                      <div className="h-1 overflow-hidden rounded-full bg-muted">
                        <div
                          className={over ? "h-full bg-destructive" : ratio >= 0.85 ? "h-full bg-warning" : "h-full bg-primary"}
                          style={{ width: `${Math.round(ratio * 100)}%` }}
                        />
                      </div>
                    </div>
                  </SettingsRow>
                );
              })}
              {value.window_ends_at === null ? null : (
                <p className="px-4 py-3 text-xs text-muted-foreground">The measurement window resets {day(value.window_ends_at)}.</p>
              )}
            </>
          )
        }
      </Loadable>
    </SettingsSection>
  );
}

function PlanTab({ organization }: { readonly organization: PlatformOrganization }): React.ReactElement {
  const access = usePlatformAccess();
  if (!access.canGrant) {
    return (
      <SettingsSection title="Plan">
        <SettingsRow className="text-sm" title="Base plan" control={organization.plan ?? "—"} />
        <SettingsRow className="text-sm"
          title="Complimentary grants"
          description="Only entitlement administrators can see grant details and change plans."
          control={organization.grants === null ? "—" : String(organization.grants)}
        />
      </SettingsSection>
    );
  }
  if (organization.state !== "ready") {
    return (
      <SettingsSection title="Plan">
        <EmptyNote>Plans can be changed once the organization is ready.</EmptyNote>
      </SettingsSection>
    );
  }
  return <OrganizationPlan organization={organization} csrf={access.csrf} />;
}

function BillingTab({ id }: { readonly id: string }): React.ReactElement {
  const api = usePlatformApi();
  const billing = useResource<OrganizationBilling>(() => api.billing(id), [api, id]);
  return (
    <SettingsSection title="Billing">
      <Loadable resource={billing} label="billing">
        {(value) =>
          !value.enabled ? (
            <EmptyNote>Billing is off for this organization. It runs on its plan and grants alone.</EmptyNote>
          ) : (
            <>
              <SettingsRow className="text-sm" title="Status" control={<StateLabel state={value.status} />} />
              <SettingsRow className="text-sm" title="Stripe mode" control={value.mode === null ? "—" : <Badge variant={value.mode === "live" ? "success" : "warning"}>{value.mode}</Badge>} />
              <SettingsRow className="text-sm" title="Customer" control={<span className="font-mono text-xs">{value.customer_id ?? "Not created yet"}</span>} />
              <SettingsRow className="text-sm" title="Paid through" control={day(value.paid_through)} />
              {value.grace_until === null ? null : (
                <SettingsRow className="text-sm" title="Grace ends" description="Work continues until then; afterwards the organization drops to its free allowances." control={day(value.grace_until)} />
              )}
              <SettingsRow className="text-sm" title="Undelivered Stripe events" control={<span className="tabular-nums">{value.pending_events}</span>} />
            </>
          )
        }
      </Loadable>
    </SettingsSection>
  );
}

function ProvisioningTab({ id }: { readonly id: string }): React.ReactElement {
  const api = usePlatformApi();
  const history = useResource<ProvisioningHistory>(() => api.provisioning(id), [api, id]);
  return (
    <SettingsSection title="Provisioning history">
      <Loadable resource={history} label="provisioning history">
        {(value) =>
          value.events.length === 0 ? (
            <EmptyNote>No transitions are recorded.</EmptyNote>
          ) : (
            <ol aria-label="Provisioning history" className="divide-y divide-border/50">
              {value.events.map((event, index) => (
                <li key={`${event.at}-${index}`} className="flex items-start gap-3 px-3 py-3 sm:px-4">
                  <span className="mt-0.5 w-40 shrink-0 text-xs text-muted-foreground">
                    <Ago at={event.at} />
                  </span>
                  <span className="min-w-0 flex-1 text-sm">
                    <StateLabel state={event.to} />
                    <span className="text-muted-foreground">
                      {event.step === "" ? "" : ` at ${event.step.replaceAll("_", " ")}`}
                      {event.error_code === "" ? "" : `, ${event.error_code.replaceAll("_", " ")}`}
                    </span>
                  </span>
                  <span className="shrink-0 text-xs text-muted-foreground">{event.actor}</span>
                </li>
              ))}
            </ol>
          )
        }
      </Loadable>
    </SettingsSection>
  );
}

function AuditTab({ id }: { readonly id: string }): React.ReactElement {
  const api = usePlatformApi();
  const page = useResource<AuditPage>(() => api.audit({ organization: id }), [api, id]);
  return (
    <SettingsSection title="Audit">
      <Loadable resource={page} label="audit entries">
        {(value) => <AuditTable entries={value.entries} showOrganization={false} />}
      </Loadable>
    </SettingsSection>
  );
}

function OrganizationActions({
  organization,
  detail,
}: {
  readonly organization: PlatformOrganization;
  readonly detail: OrganizationDetail | undefined;
}): React.ReactElement {
  const api = usePlatformApi();
  const access = usePlatformAccess();
  const csrf = access.csrf;
  return (
    <div className="flex flex-wrap items-start gap-2 sm:justify-end">
      {organization.state === "failed" ? (
        <ReasonDialog
          trigger="Retry provisioning"
          title={`Retry provisioning ${organization.name}?`}
          description="Provisioning resumes from the last completed step. The creator sees the progress page again."
          confirm="Retry provisioning"
          disabled={detail !== undefined && !detail.can_retry}
          disabledReason="This failure is terminal and needs operator repair."
          onConfirm={async (reason) =>
            (await api.retryProvisioning({ organization: organization.id, csrf, reason })).message
          }
        />
      ) : null}
      {organization.state === "disabled" ? (
        <ReasonDialog
          trigger="Reactivate"
          title={`Reactivate ${organization.name}?`}
          description="Members can sign in again and runners resume taking work."
          confirm="Reactivate"
          onConfirm={async (reason) => (await api.reactivate({ organization: organization.id, csrf, reason })).message}
        />
      ) : organization.state === "ready" ? (
        <ReasonDialog
          trigger="Suspend"
          title={`Suspend ${organization.name}?`}
          description="Every session ends, sign-in is refused and runners stop taking work. Data is kept and billing is unchanged."
          confirm="Suspend organization"
          destructive
          onConfirm={async (reason) => (await api.suspend({ organization: organization.id, csrf, reason })).message}
        />
      ) : null}
      <StartSupportDialog organization={organization} csrf={csrf} allowed={organization.can_support} />
    </div>
  );
}

export function OrganizationDetailPage({
  organization: id,
  tab,
}: {
  readonly organization: string;
  readonly tab: OrganizationTab;
}): React.ReactElement {
  const api = usePlatformApi();
  const navigate = useNavigate();
  const listing = useResource<PlatformOrganizations>(() => api.live.organizations(), [api]);
  const detail = useResource<OrganizationDetail>(() => api.organization(id), [api, id]);
  const organization = listing.value?.organizations.find((candidate) => candidate.id === id);
  const base = `/platform/organizations/${encodeURIComponent(id)}`;
  const name = organization?.name ?? detail.value?.name ?? id;

  let body: React.ReactNode;
  if (listing.value === undefined) {
    body =
      listing.error === null ? (
        <LoadingRows label="Loading the organization" />
      ) : (
        <FailedState error={listing.error} onRetry={() => void listing.refresh()} />
      );
  } else if (organization === undefined) {
    body = <EmptyNote>No organization {id} is registered. It may have been deleted.</EmptyNote>;
  } else {
    body = (
      <>
        <div className="flex flex-wrap items-start gap-4">
          <div className="min-w-64 flex-1">
            <h2 className="text-2xl font-semibold tracking-[-0.02em]">{organization.name}</h2>
            <p className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-muted-foreground">
              <StateLabel state={organization.state} />
              <span className="font-mono text-xs">{organization.id}</span>
              <span>{organization.plan ?? "Plan not reported"}</span>
            </p>
          </div>
          <OrganizationActions organization={organization} detail={detail.value} />
        </div>
        <div className="-mx-1 overflow-x-auto px-1">
          <ToggleGroup
            aria-label="Organization section"
            variant="segmented"
            value={[tab]}
            onValueChange={(next) => {
              const value = next[0];
              if (value !== undefined) void navigate({ to: value === "overview" ? base : `${base}/${value}` } as never);
            }}
          >
            {ORGANIZATION_TABS.map((option) => (
              <Toggle key={option.id} value={option.id}>
                {option.label}
              </Toggle>
            ))}
          </ToggleGroup>
        </div>
        {tab === "overview" ? (
          <Loadable resource={detail} label="the organization">
            {(value) => <OverviewTab detail={value} />}
          </Loadable>
        ) : null}
        {tab === "members" ? <MembersTab id={id} /> : null}
        {tab === "projects" ? <ProjectsTab id={id} /> : null}
        {tab === "runners" ? <RunnersTab id={id} /> : null}
        {tab === "usage" ? <UsageTab id={id} /> : null}
        {tab === "plan" ? <PlanTab organization={organization} /> : null}
        {tab === "billing" ? <BillingTab id={id} /> : null}
        {tab === "provisioning" ? <ProvisioningTab id={id} /> : null}
        {tab === "audit" ? <AuditTab id={id} /> : null}
      </>
    );
  }

  const label = ORGANIZATION_TABS.find((option) => option.id === tab)?.label ?? "Overview";
  return (
    <PlatformPage
      crumbs={[
        { label: "Organizations", to: "/platform/organizations" },
        ...(tab === "overview" ? [{ label: name }] : [{ label: name, to: base }, { label }]),
      ]}
      proposed={tab !== "plan"}
    >
      {body}
    </PlatformPage>
  );
}
