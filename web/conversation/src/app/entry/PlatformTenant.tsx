import React from "react";
import {
  Link,
  useNavigate,
  useParams,
  useRouterState,
} from "@tanstack/react-router";
import { MoreHorizontal } from "lucide-react";

import { useSidebar } from "../../components/ui/sidebar.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
  DialogTrigger,
} from "../../components/ui/dialog.tsx";
import {
  AlertDialog,
  AlertDialogClose,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogPopup,
  AlertDialogTitle,
} from "../../components/ui/alert-dialog.tsx";
import { Label } from "../../components/ui/label.tsx";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import {
  Menu,
  MenuItem,
  MenuPopup,
  MenuTrigger,
} from "../../components/ui/menu.tsx";
import { ToggleGroup, Toggle } from "../../components/ui/toggle-group.tsx";
import { UsageMeter } from "../../components/ui/usage-meter.tsx";
import { SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import { validateTenantSearch, type TenantSearch } from "./tenantList.ts";
import { planUsageRows } from "../settings/planUsage.ts";
import { ControlError } from "../account/controls.tsx";
import { useMutation } from "../account/useResource.ts";
import { usePageTitle } from "../pageTitle.ts";
import {
  SUPPORT_REASONS,
  type PlatformOrganization,
  type PlatformTenantDetail,
} from "./api.ts";
import { GrantAICreditsDialog, TenantCredits } from "./TenantCredits.tsx";
import { GrantComplimentaryDialog, TenantPlan } from "./ComplimentaryPlans.tsx";
import { Problem, useEntryApi } from "./EntryScreens.tsx";
import {
  PlatformHeadingContext,
  PlatformPageActions,
} from "./PlatformPageActions.tsx";
import {
  auditDetail,
  planDisplayName,
  platformTime,
  readableKey,
  SUPPORT_REASON_LABELS,
  tenantPlanGrant,
} from "./platformFormat.ts";
import {
  redirectPlatformSignIn,
  usePlatformResource,
} from "./usePlatformResource.ts";

const SECTIONS = [
  ["overview", "Overview"],
  ["billing", "Plan and billing"],
  ["credits", "AI credits"],
  ["members", "Members"],
  ["runners", "Runners"],
  ["projects", "Projects"],
  ["activity", "Activity"],
] as const;

function SupportAccess({
  organization,
  csrf,
}: {
  readonly organization: PlatformOrganization;
  readonly csrf: string;
}): React.ReactElement {
  const [reason, setReason] = React.useState<string>(SUPPORT_REASONS[0]);
  const [note, setNote] = React.useState("");
  const [pending, setPending] = React.useState(false);
  return (
    <Dialog
      onOpenChange={(open) => {
        if (open) {
          setReason(SUPPORT_REASONS[0]);
          setNote("");
          setPending(false);
        }
      }}
    >
      <DialogTrigger render={<Button size="sm" variant="outline" />}>
        Support access
      </DialogTrigger>
      <DialogPopup>
        <form
          className="contents"
          method="post"
          action="/support/start"
          onSubmit={() => setPending(true)}
        >
          <input type="hidden" name="organization" value={organization.id} />
          <input type="hidden" name="csrf" value={csrf} />
          <input type="hidden" name="reason" value={reason} />
          <DialogHeader>
            <DialogTitle>Start support access</DialogTitle>
            <DialogDescription>
              You will enter {organization.name} as a support viewer. The
              session is time-limited and recorded in the audit log.
            </DialogDescription>
          </DialogHeader>
          <DialogPanel className="grid gap-4">
            <div className="grid gap-1.5">
              <Label htmlFor="support-reason">Reason</Label>
              <Select
                value={reason}
                items={SUPPORT_REASONS.map((value) => ({
                  value,
                  label: SUPPORT_REASON_LABELS[value],
                }))}
                onValueChange={(value) =>
                  setReason(value ?? SUPPORT_REASONS[0])
                }
              >
                <SelectTrigger id="support-reason" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectPopup>
                  {SUPPORT_REASONS.map((value) => (
                    <SelectItem key={value} value={value}>
                      {SUPPORT_REASON_LABELS[value]}
                    </SelectItem>
                  ))}
                </SelectPopup>
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="support-note">Note (optional)</Label>
              <Textarea
                id="support-note"
                name="note"
                maxLength={500}
                placeholder="Ticket link or context"
                value={note}
                onChange={(event) => setNote(event.target.value)}
              />
            </div>
            <p className="text-sm text-muted-foreground">
              Ends automatically within one hour. End it sooner from the banner
              shown inside the tenant.
            </p>
          </DialogPanel>
          <DialogFooter>
            <DialogClose
              render={<Button variant="outline" disabled={pending} />}
            >
              Cancel
            </DialogClose>
            <Button type="submit" disabled={pending}>
              {pending ? "Starting…" : "Start support access"}
            </Button>
          </DialogFooter>
        </form>
      </DialogPopup>
    </Dialog>
  );
}

function TenantActions({
  detail,
  role,
  onChanged,
  onGrantCredits,
}: {
  readonly onGrantCredits: () => void;
  readonly detail: PlatformTenantDetail;
  readonly role: string;
  readonly onChanged: () => Promise<void>;
}): React.ReactElement {
  const api = useEntryApi();
  const [resumeOpen, setResumeOpen] = React.useState(false);
  const resume = useMutation(() =>
    api.resumePlatformTenant({
      organization: detail.organization.id,
      csrf: detail.csrf,
    }),
  );
  React.useEffect(() => {
    redirectPlatformSignIn(resume.error);
  }, [resume.error]);
  const canGrant =
    (role === "billing" || role === "admin") &&
    detail.can_grant &&
    detail.organization.state === "ready";
  const canResume =
    role === "admin" &&
    detail.can_resume &&
    detail.organization.state === "failed";
  return (
    <>
      {(role === "support" || role === "admin") &&
      detail.organization.can_support &&
      detail.organization.state === "ready" ? (
        <SupportAccess organization={detail.organization} csrf={detail.csrf} />
      ) : null}
      {canGrant ? <Button size="sm" onClick={onGrantCredits}>Grant AI credits</Button> : null}

      {canGrant || canResume ? (
        <Menu>
          <MenuTrigger
            render={
              <Button
                size="icon-sm"
                variant="outline"
                aria-label="Tenant actions"
              />
            }
          >
            <MoreHorizontal />
          </MenuTrigger>
          <MenuPopup keepMounted>
            {canResume ? (
              <MenuItem
                onClick={() => {
                  resume.clearError();
                  setResumeOpen(true);
                }}
              >
                Resume provisioning
              </MenuItem>
            ) : null}
            {canGrant && detail.entitlements ? (
              <GrantComplimentaryDialog
                organization={detail.organization}
                entitlements={detail.entitlements}
                csrf={detail.csrf}
                onChanged={onChanged}
                modelChoice
                trigger={(open, disabled) => (
                  <MenuItem disabled={disabled} onClick={open}>
                    Grant model choice
                  </MenuItem>
                )}
              />
            ) : null}
          </MenuPopup>
        </Menu>
      ) : null}
      <AlertDialog open={resumeOpen} onOpenChange={setResumeOpen}>
        <AlertDialogPopup>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Resume provisioning for {detail.organization.name}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              Continue setup from the last completed step.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <ControlError message={resume.error?.message ?? null} />
          <AlertDialogFooter>
            <AlertDialogClose
              render={<Button variant="outline" disabled={resume.pending} />}
            >
              Cancel
            </AlertDialogClose>
            <Button
              disabled={resume.pending}
              onClick={async () => {
                if ((await resume.call()) !== null) {
                  await onChanged();
                  setResumeOpen(false);
                }
              }}
            >
              {resume.pending ? "Resuming…" : "Resume"}
            </Button>
          </AlertDialogFooter>
        </AlertDialogPopup>
      </AlertDialog>
    </>
  );
}

function TenantUsage({
  detail,
}: {
  readonly detail: PlatformTenantDetail;
}): React.ReactElement {
  const value = detail.entitlements;
  const grant = value ? tenantPlanGrant(value) : undefined;
  const name = value
    ? planDisplayName(grant?.plan ?? value.effective_base, value)
    : "current plan";
  const usage = detail.usage;
  const allowances = Object.fromEntries(
    Object.entries(usage?.allowances ?? {}).map(([key, row]) => [
      key,
      row.limit ?? 0,
    ]),
  );
  if (usage && Object.keys(allowances).length > 0) {
    allowances.artifact_retained_bytes ??= 0;
  }
  const rows = usage
    ? planUsageRows({
        allowances,
        usage: Object.fromEntries(
          Object.entries(usage.allowances).map(([key, row]) => [key, row.used]),
        ),
      })
    : [];
  return (
    <SettingsSection
      title={`Usage against ${name}`}
      aria-label="Tenant usage"
      headerAction={
        usage?.window_ends_at ? (
          <span className="text-xs text-muted-foreground">
            Hourly limits reset at {platformTime(usage.window_ends_at)}
          </span>
        ) : null
      }
    >
      {!usage || rows.length === 0 ? (
        <SettingsRow
          title="Usage"
          control={
            <span className="text-sm text-muted-foreground">Unavailable</span>
          }
        />
      ) : (
        <>
          {rows.map((row) => (
            <div key={row.name} className="px-4 py-3">
              <UsageMeter
                label={row.label}
                used={row.used}
                limit={row.limit}
                unit={row.unit}
                limitOnly={row.limitOnly}
                unlimited={usage.allowances[row.name]?.limit === null}
                description={row.description}
                notIncludedOn={
                  row.name.startsWith("artifact_") &&
                  value !== null &&
                  !(value.features ?? []).includes("hosted_artifacts")
                    ? name
                    : undefined
                }
              />
            </div>
          ))}
        </>
      )}
    </SettingsSection>
  );
}

function TenantActivity({
  detail,
  recent = false,
}: {
  readonly detail: PlatformTenantDetail;
  readonly recent?: boolean;
}): React.ReactElement {
  const api = useEntryApi();
  const query = `tenant=${encodeURIComponent(detail.organization.id)}${recent ? "&limit=3" : ""}`;
  const audit = usePlatformResource(
    () => api.platformAudit(query),
    [api, query],
  );
  return (
    <SettingsSection
      title={recent ? "Recent activity" : "Audit activity"}
      aria-label={recent ? "Recent activity" : "Audit activity"}
      headerAction={
        <Button
          variant="outline"
          size="sm"
          render={
            <Link
              to={"/platform/audit" as never}
              search={{ tenant: detail.organization.id } as never}
            />
          }
        >
          Open audit log
        </Button>
      }
    >
      <Problem message={audit.error?.message ?? null} />
      {!audit.value ? (
        audit.loading ? (
          <Skeleton className="h-20" aria-label="Loading audit activity" />
        ) : null
      ) : audit.value.rows.length === 0 ? (
        <SettingsRow title="No audit events." />
      ) : (
        audit.value.rows.map((row) => (
          <SettingsRow
            key={`${row.source}-${row.id}`}
            title={readableKey(row.event)}
            description={[
              row.actor,
              auditDetail(row.detail, detail.entitlements ?? undefined),
            ]
              .filter(Boolean)
              .join(" · ")}
            control={
              <time dateTime={row.at} className="text-xs text-muted-foreground">
                {platformTime(row.at)}
              </time>
            }
          />
        ))
      )}
    </SettingsSection>
  );
}

export function PlatformTenant({
  role,
}: {
  readonly role: string;
}): React.ReactElement {
  const { organization } = useParams({ strict: false }) as {
    organization: string;
  };
  const api = useEntryApi();
  const [creditDialogOpen, setCreditDialogOpen] = React.useState(false);
  const [creditRevision, setCreditRevision] = React.useState(0);
  const creditTrigger = React.useRef<HTMLElement | null>(null);
  const openCreditDialog = () => {
    creditTrigger.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setCreditDialogOpen(true);
  };
  const { isMobile } = useSidebar();
  const resource = usePlatformResource(
    () => api.platformTenant(organization),
    [api, organization],
  );
  const search = useRouterState({
    select: (state) => state.location.search as TenantSearch & { section?: string },
  });
  const selected =
    SECTIONS.find(([id]) => id === search.section)?.[0] ?? "overview";
  const navigate = useNavigate();
  const setHeading = React.useContext(PlatformHeadingContext);
  const tenant = resource.value?.organization;
  usePageTitle(tenant?.name ?? "Tenant", "Platform");
  React.useEffect(() => {
    setHeading(
      <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
        <Link
          to={"/platform/tenants" as never}
          search={validateTenantSearch(search) as never}
          className="text-muted-foreground"
        >
          Tenants
        </Link>
        <span aria-hidden="true">/</span>
        <h1 className="min-w-0 truncate font-medium">
          {tenant?.name ?? "Tenant"}
        </h1>
        {tenant ? (
          <Badge
            variant={
              tenant.state === "ready"
                ? "success"
                : tenant.state === "failed"
                  ? "error"
                  : "outline"
            }
          >
            {readableKey(tenant.state)}
          </Badge>
        ) : null}
      </div>,
    );
    return () => setHeading(null);
  }, [setHeading, tenant?.name, tenant?.state, search]);
  const detail = resource.value;
  const owner =
    detail?.members?.find((member) => member.role === "owner")?.email ??
    (detail?.members === null ? tenant?.creator_email : undefined);
  const canGrant =
    (role === "billing" || role === "admin") &&
    detail?.can_grant === true &&
    tenant?.state === "ready";
  return (
    <div className="flex min-w-0 flex-col gap-6">
      <Problem
        message={
          resource.error
            ? `This tenant could not be reloaded and may be out of date: ${resource.error.message}`
            : null
        }
      />
      {!detail ? (
        resource.loading ? (
          <Skeleton className="h-48" aria-label="Loading tenant" />
        ) : null
      ) : (
        <>
          {isMobile ? (
            <div className="flex flex-wrap items-center gap-2">
              <TenantActions
                key={organization}
                detail={detail}
                role={role}
                onChanged={resource.refresh}
                onGrantCredits={openCreditDialog}
              />
            </div>
          ) : (
            <PlatformPageActions>
              <TenantActions
                key={organization}
                detail={detail}
                role={role}
                onChanged={resource.refresh}
                onGrantCredits={openCreditDialog}
              />
            </PlatformPageActions>
          )}
          {canGrant ? <GrantAICreditsDialog key={organization} detail={detail} returnFocus={creditTrigger} open={creditDialogOpen} onOpenChange={setCreditDialogOpen}
            onChanged={async (balance) => {
              resource.set({ ...detail, credit_balance_micros: balance, ai_credits: detail.ai_credits ? { ...detail.ai_credits, balance_micros: balance } : null });
              await resource.refresh();
              setCreditRevision((value) => value + 1);
            }} /> : null}
          <div className="min-w-0 overflow-x-auto">
            <ToggleGroup
              aria-label="Tenant section"
              value={[selected]}
              onValueChange={(values) => {
                if (values[0])
                  void navigate({
                    to: "/platform/tenants/$organization",
                    params: { organization },
                    search: { ...validateTenantSearch(search), section: values[0] },
                  } as never);
              }}
            >
              {SECTIONS.map(([id, title]) => (
                <Toggle key={id} value={id}>
                  {title}
                  {id === "members" || id === "runners" || id === "projects" ? (
                    detail[id] ? (
                      <Badge variant="outline">{detail[id].length}</Badge>
                    ) : null
                  ) : null}
                </Toggle>
              ))}
            </ToggleGroup>
          </div>
          {selected === "overview" ? (
            <>
              <SettingsSection title="Organization" aria-label="Organization">
                <SettingsRow
                  title="Organization ID"
                  control={
                    <>
                      <span className="break-all font-mono text-xs">
                        {organization}
                      </span>
                      <CopyOrganization key={organization} id={organization} />
                    </>
                  }
                />
                <SettingsRow
                  title="Owner"
                  control={
                    owner ? (
                      <Link
                        to={"/platform/users/$email" as never}
                        params={{ email: owner } as never}
                        className="break-all text-sm text-link underline"
                      >
                        {owner}
                      </Link>
                    ) : (
                      <span className="text-sm text-muted-foreground">
                        Registered externally
                      </span>
                    )
                  }
                />
                <SettingsRow
                  title="Created"
                  control={
                    <time
                      dateTime={detail.organization.created_at}
                      className="text-sm"
                    >
                      {platformTime(detail.organization.created_at)}
                    </time>
                  }
                />
              </SettingsSection>
              {detail.entitlements ? (
                <TenantPlan
                  organization={detail.organization}
                  csrf={detail.csrf}
                  value={detail.entitlements}
                  canGrant={canGrant}
                  onChanged={resource.refresh}
                />
              ) : (
                <SettingsSection title="Plan">
                  <SettingsRow title="Plan" control="Unavailable" />
                </SettingsSection>
              )}
              <TenantCredits detail={detail} canGrant={canGrant} onGrant={openCreditDialog} />
              <TenantUsage detail={detail} />
              <TenantActivity key={creditRevision} detail={detail} recent />
            </>
          ) : selected === "billing" ? (
            detail.entitlements ? (
              <TenantPlan
                organization={detail.organization}
                csrf={detail.csrf}
                value={detail.entitlements}
                canGrant={canGrant}
                onChanged={resource.refresh}
                history
              />
            ) : (
              <SettingsSection title="Plan and billing">
                <SettingsRow title="Plan" control="Unavailable" />
              </SettingsSection>
            )
          ) : selected === "credits" ? (
            <TenantCredits detail={detail} canGrant={canGrant} onGrant={openCreditDialog} history />
          ) : selected === "activity" ? (
            <>
              <TenantActivity key={creditRevision} detail={detail} />
              <SettingsSection
                title="Provisioning timeline"
                aria-label="Provisioning timeline"
              >
                {detail.events.length === 0 ? (
                  <SettingsRow title="No provisioning events." />
                ) : (
                  detail.events.map((event, index) => (
                    <SettingsRow
                      key={`${event.recorded_at}-${index}`}
                      title={readableKey(event.event)}
                      control={
                        <time
                          dateTime={event.recorded_at}
                          className="text-xs text-muted-foreground"
                        >
                          {platformTime(event.recorded_at)}
                        </time>
                      }
                    />
                  ))
                )}
              </SettingsSection>
            </>
          ) : (
            <SettingsSection
              title={SECTIONS.find(([id]) => id === selected)![1]}
              aria-label={SECTIONS.find(([id]) => id === selected)![1]}
            >
              {detail[selected] === null ? (
                <SettingsRow title="Unavailable" />
              ) : detail[selected].length === 0 ? (
                <SettingsRow title={`No ${selected}.`} />
              ) : selected === "members" ? (
                detail.members!.map((member) => (
                  <SettingsRow
                    key={member.id}
                    title={
                      <Link
                        to={"/platform/users/$email" as never}
                        params={{ email: member.email } as never}
                        className="break-all text-link underline"
                      >
                        {member.email}
                      </Link>
                    }
                    control={
                      <Badge variant="outline">
                        {readableKey(member.role)}
                      </Badge>
                    }
                  />
                ))
              ) : selected === "runners" ? (
                detail.runners!.map((runner) => (
                  <SettingsRow
                    key={runner.id}
                    title={runner.name || "Unnamed runner"}
                    control={
                      <Badge
                        variant={
                          runner.health === "online" ? "success" : "outline"
                        }
                      >
                        {readableKey(runner.health)}
                      </Badge>
                    }
                  />
                ))
              ) : (
                detail.projects!.map((project) => (
                  <SettingsRow key={project.id} title={project.name} />
                ))
              )}
            </SettingsSection>
          )}
        </>
      )}
    </div>
  );
}

function CopyOrganization({ id }: { readonly id: string }): React.ReactElement {
  const [copied, setCopied] = React.useState(false);
  const [error, setError] = React.useState(false);
  return (
    <>
      <Button
        size="sm"
        variant="outline"
        onClick={async () => {
          try {
            await navigator.clipboard.writeText(id);
            setCopied(true);
            setError(false);
          } catch {
            setError(true);
          }
        }}
      >
        {copied ? "Copied" : "Copy"}
      </Button>
      {error ? (
        <span role="alert" className="text-xs text-error-foreground">
          Copy failed. Select the ID to copy it.
        </span>
      ) : null}
    </>
  );
}
