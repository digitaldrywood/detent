import { SHELL_SHORTCUTS } from "../lib/shortcuts.ts";
import {
  ArrowRightIcon,
  CheckIcon,
  ClockIcon,
  LockKeyholeIcon,
  TriangleAlertIcon,
  BlocksIcon,
  CreditCardIcon,
  InfoIcon,
  KeyboardIcon,
  PanelsTopLeftIcon,
  ReceiptTextIcon,
  Settings2Icon,
} from "lucide-react";
import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Switch } from "../../components/ui/switch.tsx";
import { UsageMeter, formatUsageValue } from "../../components/ui/usage-meter.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Alert, AlertDescription, AlertTitle } from "../../components/ui/alert.tsx";
import { Kbd } from "../../components/ui/kbd.tsx";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import type { BillingReport, BillingUsageReport, ComparisonPlan, PlanReport, ProjectsResponse } from "../../contracts/account.ts";
import { ControlError } from "../account/controls.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { PLATFORM } from "../entry/api.ts";
import { newKey } from "../account/idempotency.ts";
import { OrganizationMembersRoute } from "../account/Organization.tsx";
import { OrganizationOverviewRoute, OrganizationModelsRoute } from "../account/OrganizationOverview.tsx";
import { ProjectSettingsRoute } from "../account/ProjectSettings.tsx";
import { useMutation, useResource } from "../account/useResource.ts";
import { RunnersSettings } from "../fleet/RunnersSection.tsx";
import { setupStepsLeftLabel } from "../work/components/FirstRun.tsx";
import { useNewProject } from "../projects/NewProject.tsx";
import { usePageTitle } from "../pageTitle.ts";
import { keybindingCatalogue } from "../adapters/keybindings.ts";
import {
  DEFAULT_SECTION,
  isSettingsSectionId,
  SETTINGS_SECTION_LABELS,
  settingsNavItems,
  type SettingsSectionId,
} from "./sections.tsx";
import { CreditSettings } from "./CreditSettings.tsx";
import { approachingPlanLimits, nextFittingPlan, planUsageRows, planStatus } from "./planUsage.ts";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { SettingsHelp } from "./SettingsHelp.tsx";
import { cn } from "../../lib/utils.ts";
import { formatCount, formatUsd } from "../usage/usageFormat.ts";
import { SettingsPageContainer, SettingsRow, SettingsSection } from "./settingsLayout.tsx";
import { ProjectRankSettings } from "./ProjectRankSettings.tsx";
import { SpritesCard } from "../account/SpritesCard.tsx";
import { SlackSettings } from "./SlackSettings.tsx";
import { WhatsNewSettings } from "./WhatsNewSettings.tsx";
import { MCPSettings } from "./MCPSettings.tsx";
import { basePath, signInPath } from "../../runtime/basePath.ts";

/** The allowance label the plan report's keys become. */
export function allowanceLabel(name: string): string {
  return name.replace(/^api_/, "API ").replaceAll("_", " ");
}



/** An allowance row's three facts: consumed, allowed, and whether it is over. */
export interface AllowanceRow {
  readonly name: string;
  readonly label: string;
  readonly used: number;
  readonly limit: number;
  readonly overLimit: boolean;
}

export function allowanceRows(plan: PlanReport): readonly AllowanceRow[] {
  const names = new Set(Object.keys(plan.allowances));
  return [...names].toSorted().map((name) => {
    const used = plan.usage[name] ?? 0;
    const limit = plan.allowances[name] ?? 0;
    return { name, label: allowanceLabel(name), used, limit, overLimit: used > limit };
  });
}

export function AllowanceList({ rows }: { readonly rows: readonly AllowanceRow[] }): React.ReactElement {
  return (
    <ul className="grid grid-cols-1 gap-x-8 gap-y-2 py-3 sm:grid-cols-2">
      {rows.map((row) => (
        <li key={row.name} className="flex items-baseline justify-between gap-3 text-sm">
          <span className="capitalize text-muted-foreground">{row.label}</span>
          <span
            className={
              row.overLimit
                ? "font-medium tabular-nums text-warning-foreground"
                : "tabular-nums text-foreground"
            }
          >
            {row.used.toLocaleString()} / {row.limit.toLocaleString()}
            {row.overLimit ? <span className="ps-1.5 text-xs">over limit</span> : null}
            {row.name === "concurrent_work" || row.name === "unarchived_issues" ? <span className="ps-1.5 text-xs">{Math.max(0, row.limit - row.used).toLocaleString()} remaining</span> : null}
          </span>
        </li>
      ))}
    </ul>
  );
}

// --- Sections ---------------------------------------------------------------

/** General: what this session is, and which organization it is acting on. */
export function GeneralSettings(): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const api = useAccountApi();
  const notifications = useResource(() => api.issueNotificationPreference(), [api]);
  const saveNotifications = useMutation(async (enabled: boolean) => {
    const value = await api.saveIssueNotificationPreference(enabled);
    notifications.set(value);
    return value;
  });
  const signOut = useMutation(async () => {
    await api.logout();
    globalThis.location?.assign(signInPath());
    return null;
  });

  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-general"
        title="General"
        icon={<Settings2Icon className="size-3.5" />}
      >
        <SettingsRow
          id="settings-general-organization"
          title="Organization"
          description="The organization every project, runner and conversation on this session belongs to."
          control={
            <span className="text-sm text-foreground">
              {bootstrap?.organization.name ?? "Not signed in"}
            </span>
          }
        />
        <SettingsRow
          id="settings-general-account"
          title="Your account"
          description={bootstrap?.actor.email ?? "This hub does not report an actor."}
          status={bootstrap === null ? undefined : `Role: ${bootstrap.actor.role}`}
          control={
            <div className="flex flex-wrap items-center gap-2">
              {bootstrap?.actor.platform_role ? (
                <Button size="sm" variant="outline" render={<a href={PLATFORM} />}>
                  Platform console
                </Button>
              ) : null}
              <Button
                size="sm"
                variant="outline"
                disabled={signOut.pending}
                onClick={() => void signOut.call()}
              >
                {signOut.pending ? "Signing out…" : "Sign out"}
              </Button>
            </div>
          }
        />
        {bootstrap?.actor.role === "owner" || bootstrap?.actor.role === "admin" ? <ProjectRankSettings /> : null}
        <SettingsRow
          id="settings-general-issue-notifications"
          title="Issue email notifications"
          description="Notify me about issues I filed or commented on."
          control={<Switch
            aria-label="Notify me about issues I filed or commented on"
            checked={notifications.value?.enabled ?? false}
            disabled={notifications.loading || notifications.error !== null || saveNotifications.pending}
            onCheckedChange={(enabled) => void saveNotifications.call(enabled)}
          />}
        />
        <ControlError message={(notifications.error ?? saveNotifications.error)?.message ?? null} />
        {bootstrap?.plan == null ? null : (
          <SettingsRow
            id="settings-general-plan"
            title="Plan"
            description={bootstrap.plan.name}
            status={`Usage window ends ${bootstrap.plan.window_ends_at}`}
          />
        )}
      </SettingsSection>
    </SettingsPageContainer>
  );
}

export function ProjectsSettings({
  onNavigate,
}: {
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const canManage = bootstrap?.actor.can_manage ?? false;
  const projects = useResource<ProjectsResponse>(() => api.projects(), [api]);
  const { openNewProject } = useNewProject();

  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-projects"
        title="Projects"
        icon={<PanelsTopLeftIcon className="size-3.5" />}
        headerAction={
          canManage ? (
            <Button size="xs" variant="outline" onClick={openNewProject}>
              New project
            </Button>
          ) : null
        }
      >
        {projects.loading && projects.value === undefined ? (
          <SettingsRow title="Loading the projects" />
        ) : projects.error !== null ? (
          <SettingsRow
            title="Projects are not available"
            description={
              projects.error.isAccessError
                ? "You do not have access to this organization's projects."
                : projects.error.message
            }
            control={
              <Button size="sm" variant="outline" onClick={() => void projects.refresh()}>
                Try again
              </Button>
            }
          />
        ) : (projects.value ?? []).length === 0 ? (
          <SettingsRow
            title="No projects yet"
            description="A project is where issues, runners and a repository binding live."
          />
        ) : (
          (projects.value ?? []).map((project) => (
            <SettingsRow
              key={project.id}
              title={project.name}
              description={`${project.profile} · ${project.states.length} workflow states`}
              status={
                project.onboarding.ready
                  ? "Set up"
                  : setupStepsLeftLabel(project.onboarding.steps.filter((step) => step.state !== "ready").length)
              }
              control={
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => onNavigate?.(`/settings/integrations?project=${project.id}`)}
                  >
                    Settings
                  </Button>
                  {project.onboarding.ready ? null : (
                    <Button size="sm" onClick={() => onNavigate?.(`/projects/${project.id}/setup`)}>
                      Finish setup
                    </Button>
                  )}
                </div>
              }
            />
          ))
        )}
      </SettingsSection>
    </SettingsPageContainer>
  );
}

export function IntegrationsSettings({
  project,
  onNavigate,
}: {
  readonly project: string | null;
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const projects = bootstrap?.projects ?? [];
  const selected = project ?? projects[0]?.id ?? "";

  if (projects.length === 0) {
    return (
      <SettingsPageContainer>
        <SlackSettings />
        {bootstrap ? <SettingsSection title="Organization Sprites"><SpritesCard projectId="" canManage={bootstrap.actor.can_manage} /></SettingsSection> : null}
        <SettingsSection
          id="settings-integrations"
          title="Integrations"
          icon={<BlocksIcon className="size-3.5" />}
        >
          <SettingsRow
            title="No projects yet"
            description="An integration binds one project to one repository, so there has to be a project first."
          />
        </SettingsSection>
      </SettingsPageContainer>
    );
  }

  const picker = (
    <>
      <SlackSettings />
        {bootstrap ? <SettingsSection title="Organization Sprites"><SpritesCard projectId="" canManage={bootstrap.actor.can_manage} /></SettingsSection> : null}
      <SettingsSection
        id="settings-integrations"
        title="Integrations"
        icon={<BlocksIcon className="size-3.5" />}
      >
        <SettingsRow
          title="Project"
          description="Integration, intake, projection and policy are configured per project."
          control={
            <Select
              value={selected}
              onValueChange={(value) =>
                onNavigate?.(`/settings/integrations?project=${String(value)}`)
              }
            >
              <SelectTrigger aria-label="Project" size="sm" className="min-w-44">
                <SelectValue>
                  {projects.find((candidate) => candidate.id === selected)?.name ?? selected}
                </SelectValue>
              </SelectTrigger>
              <SelectPopup>
                {projects.map((candidate) => (
                  <SelectItem key={candidate.id} value={candidate.id}>
                    {candidate.name}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
          }
        />
      </SettingsSection>
    </>
  );

  // One scroll container, not two: the picker goes inside the project screen's
  // own `SettingsPageContainer` through its `header` slot.
  return <ProjectSettingsRoute key={selected} projectId={selected} onNavigate={onNavigate} header={picker} />;
}

export function planPriceText(plan: PlanReport): string {
  if (plan.monthly_usd_cents === undefined || plan.monthly_usd_cents === null) return "Custom or legacy complimentary access";
  return `$${(plan.monthly_usd_cents / 100).toLocaleString()} per organization/month${plan.monthly_usd_cents === 0 ? " · no card required" : ""}`;
}

export function PlanSettings(): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const plan = useResource<PlanReport>(() => api.plan(), [api]);
  const billing = useResource<BillingUsageReport>(
    () => api.billingUsage(),
    [api],
  );
  const report = plan.error === null ? plan.value : undefined;
  const usage = billing.error === null ? billing.value : undefined;
  const rows = report === undefined ? [] : planUsageRows(report);
  const excluded = rows.filter(
    (row) =>
      row.limit <= 0 ||
      (row.name.startsWith("artifact_") &&
        !report?.features?.includes("hosted_artifacts")),
  );
  const included = rows.filter((row) => !excluded.includes(row));
  const warnings = report === undefined ? [] : approachingPlanLimits(report);
  const planName = usage?.name || report?.name || "Current plan";
  const comparisons = usage?.comparison_plans ?? [];
  const currentRef = usage?.effective_base ?? report?.effective_base;
  const currentOffered = comparisons.some(
    (candidate) =>
      candidate.id === currentRef?.id &&
      candidate.version === currentRef.version,
  );
  const comparisonColumns: readonly ComparisonPlan[] =
    report === undefined || currentOffered
      ? comparisons
      : [
          {
            id: currentRef?.id ?? "",
            version: currentRef?.version ?? 1,
            name: planName,
            monthly_usd_cents:
              usage?.monthly_usd_cents ?? report.monthly_usd_cents,
            features: report.features ?? [],
            allowances: report.allowances,
            price_id: "",
          },
          ...comparisons,
        ];
  const nextPlan =
    report === undefined ? undefined : nextFittingPlan(report, comparisons);
  const canView =
    bootstrap?.actor.role === "owner" || bootstrap?.actor.role === "admin";
  const canBill =
    bootstrap?.actor.role === "owner" && bootstrap.support == null;
  const canCheckout = canBill && usage?.can_checkout === true;
  const canManage = canBill && usage?.can_manage === true;
  const purchaseKeys = React.useRef(new Map<string, string>());
  const checkout = useMutation(async (price: string) => {
    if (!canCheckout || usage?.checkout_pending) return;
    let key = purchaseKeys.current.get(price);
    if (key === undefined) {
      key = newKey();
      purchaseKeys.current.set(price, key);
    }
    const result = await api.checkout({ price, key });
    globalThis.location?.assign(result.url);
  });
  const portal = useMutation(async () => {
    if (!canManage) return;
    const result = await api.portal({ key: newKey() });
    globalThis.location?.assign(result.url);
  });
  const comparePlans = () => {
    const target = document.getElementById("settings-plan-comparison");
    target?.scrollIntoView({ block: "start" });
    target?.focus({ preventScroll: true });
  };
  const status = usage === undefined ? null : planStatus(usage);
  const expiry = usage?.expires_at
    ? `Expires ${new Date(usage.expires_at).toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" })}`
    : usage?.complimentary || currentRef?.id === "free"
      ? "Never expires"
      : status?.replace(/, \d{4}/, "");
  const reset = report?.window_ends_at
    ? new Date(report.window_ends_at).toLocaleTimeString("en-US", {
        hour: "numeric",
        minute: "2-digit",
      })
    : null;
  const moveButton = (candidate: ComparisonPlan, primary = false) =>
    canBill &&
    candidate.price_id !== "" &&
    candidate.monthly_usd_cents != null ? (
      <Button
        size="xs"
        className="w-full md:w-auto"
        variant={primary ? "default" : "outline"}
        disabled={
          !canCheckout || usage?.checkout_pending === true || checkout.pending
        }
        onClick={() => void checkout.call(candidate.price_id)}
      >
        {checkout.pending ? "Opening…" : `Move to ${candidate.name || "plan"}`}
      </Button>
    ) : null;
  const error = plan.error ?? billing.error;

  if (!canView || error?.isAccessError)
    return (
      <SettingsPageContainer>
        <SettingsSection id="settings-plan" title="Plan & usage">
          <SettingsRow
            title="Plan and usage need owner or admin access"
            description="Ask an organization owner or admin to manage your plan."
          />
        </SettingsSection>
      </SettingsPageContainer>
    );
  if (
    (plan.loading && report === undefined) ||
    (billing.loading && usage === undefined)
  )
    return (
      <SettingsPageContainer>
        <SettingsSection
          id="settings-plan"
          title="Plan"
          icon={<ReceiptTextIcon className="size-3.5" />}
        >
          <div
            role="status"
            aria-label="Loading plan and usage"
            className="space-y-4 p-4"
          >
            <Skeleton className="h-5 w-48" />
            <Skeleton className="h-4 w-64 max-w-full" />
            <Skeleton className="h-4 w-32" />
          </div>
        </SettingsSection>
      </SettingsPageContainer>
    );

  return (
    <SettingsPageContainer>
      {warnings.length > 0 ? (
        <Alert variant="warning">
          <TriangleAlertIcon className="size-4" />
          <AlertTitle>
            {currentRef?.id === "free" &&
            warnings.some((row) => row.used >= row.limit)
              ? "At your Free limit"
              : `Approaching your ${planName} limits`}
          </AlertTitle>
          <AlertDescription>
            <div className="flex flex-wrap items-center gap-2">
              {warnings.map((row) => (
                <Badge key={row.name} variant="warning">
                  {row.label} {formatUsageValue(row.used, row.unit)} /{" "}
                  {formatUsageValue(row.limit, row.unit)}
                </Badge>
              ))}
              <Button size="sm" variant="outline" onClick={comparePlans}>
                Compare plans
              </Button>
              {currentRef?.id === "free" && nextPlan
                ? moveButton(nextPlan, true)
                : null}
            </div>
          </AlertDescription>
        </Alert>
      ) : null}
      <SettingsSection
        id="settings-plan"
        title="Plan"
        icon={<ReceiptTextIcon className="size-3.5" />}
      >
        {error !== null ? (
          <SettingsRow
            title="Plan and usage are not available"
            description={error?.message}
            control={
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  void plan.refresh();
                  void billing.refresh();
                }}
              >
                Try again
              </Button>
            }
          />
        ) : report === undefined ? null : (
          <>
            <SettingsRow
              title={
                <span className="flex flex-wrap items-center gap-2">
                  {planName}
                  {usage?.complimentary ? (
                    <Badge variant="outline" size="sm">
                      Complimentary
                    </Badge>
                  ) : report.monthly_usd_cents != null ? (
                    <span className="font-normal text-muted-foreground">
                      {formatUsd(
                        (usage?.monthly_usd_cents ?? report.monthly_usd_cents) /
                          100,
                      ).replace(/\.00$/, "")}
                    </span>
                  ) : null}
                  <span className="hidden sm:inline-flex">
                    <SettingsHelp label="Plan details">
                      {planPriceText({
                        ...report,
                        monthly_usd_cents:
                          usage?.monthly_usd_cents ?? report.monthly_usd_cents,
                      })}
                    </SettingsHelp>
                  </span>
                </span>
              }
              status={
                status ? (
                  <span className="inline-flex items-center gap-1.5">
                    {usage?.complimentary && usage.expires_at ? (
                      <span className="size-1.5 rounded-full bg-success" />
                    ) : null}
                    {status}
                  </span>
                ) : null
              }
              control={
                canBill ? (
                  <div className="flex w-full gap-2 sm:w-auto">
                    <Button
                      size="sm"
                      variant="outline"
                      className="flex-1 sm:flex-none"
                      disabled={!canManage || portal.pending}
                      onClick={() => void portal.call()}
                    >
                      {portal.pending ? "Opening…" : "Manage billing"}
                    </Button>
                    <Button
                      size="sm"
                      className="flex-1 sm:flex-none"
                      onClick={comparePlans}
                    >
                      Upgrade
                    </Button>
                  </div>
                ) : null
              }
            />
            <div
              id="settings-ai-usage"
              className="flex flex-wrap items-center justify-between gap-3 px-3 py-3 text-sm sm:px-4"
            >
              <div className="flex flex-wrap items-baseline gap-2">
                <span className="text-muted-foreground">AI credits</span>
                <span className="tabular-nums">
                  {usage?.ai_credits
                    ? formatUsd(usage.ai_credits.balance_micros / 1000000)
                    : "Not enabled"}
                </span>
                {usage ? (
                  <span className="text-xs text-muted-foreground">
                    {formatUsd(usage.charged_ai_micros / 1000000)} spent this
                    period
                  </span>
                ) : null}
              </div>
              {canBill ? (
                <a
                  className="inline-flex items-center gap-1 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  href={`${basePath()}/settings/billing`}
                >
                  Billing <ArrowRightIcon className="size-3.5" />
                </a>
              ) : null}
            </div>
          </>
        )}
      </SettingsSection>
      {report === undefined || error !== null ? null : (
        <SettingsSection id="settings-plan-capacity" title="Usage">
          {["Organization", "Storage", "This hour"].map((group) => {
            const meters = included.filter((row) => row.group === group);
            return meters.length === 0 ? null : (
              <div key={group} className="px-3 py-2 sm:px-4">
                <div className="flex items-center justify-between gap-3 py-1 text-xs text-muted-foreground">
                  <h3>{group}</h3>
                  {group === "This hour" && reset ? (
                    <span className="inline-flex items-center gap-1">
                      <ClockIcon className="size-3" />
                      Resets {reset}
                    </span>
                  ) : null}
                </div>
                {meters.map((row) => (
                  <UsageMeter
                    key={row.name}
                    layout="row"
                    label={row.label}
                    used={row.used}
                    limit={row.limit}
                    unit={row.unit}
                    limitOnly={row.limitOnly}
                    description={row.description}
                    detail={
                      <SettingsHelp
                        label={row.label}
                      >{`${row.limitOnly ? `Up to ${formatUsageValue(row.limit, row.unit)}` : row.used >= row.limit ? "Limit reached" : `${formatUsageValue(Math.max(0, row.limit - row.used), row.unit)} left`}${row.description ? `. ${row.description}` : ""}`}</SettingsHelp>
                    }
                  />
                ))}
              </div>
            );
          })}
          {excluded.length > 0 ? (
            <div className="grid gap-3 px-3 py-4 sm:grid-cols-[184px_minmax(0,1fr)] sm:gap-4 sm:px-4">
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <LockKeyholeIcon className="size-3.5" />
                Not on your plan
              </div>
              <div className="flex flex-wrap gap-1.5">
                {excluded.map((row) => (
                  <Badge key={row.name} variant="secondary">
                    {row.label}
                  </Badge>
                ))}
              </div>
            </div>
          ) : null}
        </SettingsSection>
      )}
      {usage === undefined || error !== null ? null : (
        <SettingsSection
          id="settings-plan-comparison"
          title="Plans"
          headerAction={
            <span className="text-xs text-muted-foreground">
              Per organization per month
            </span>
          }
        >
          <div
            role="table"
            className="grid md:grid-flow-col md:auto-cols-fr"
            aria-label="Compare plans"
          >
            <div role="row" className="sr-only">
              <span role="columnheader">Plan</span>
              <span role="columnheader">Concurrent work</span>
              <span role="columnheader">Hosted artifacts</span>
              <span role="columnheader">Projects</span>
              <span role="columnheader">Plan action or expiry</span>
            </div>
            <div
              aria-hidden="true"
              className="hidden grid-rows-[110px_44px_44px_44px_64px] text-xs text-muted-foreground md:grid"
            >
              <div />
              <div className="flex items-center border-t border-border/50 px-3">
                Concurrent work
              </div>
              <div className="flex items-center border-t border-border/50 px-3">
                Hosted artifacts
              </div>
              <div className="flex items-center border-t border-border/50 px-3">
                Projects
              </div>
              <div className="border-t border-border/50" />
            </div>
            {comparisonColumns.map((candidate) => {
              const current =
                candidate.id === currentRef?.id &&
                candidate.version === currentRef.version;
              const custom =
                candidate.id === "scale" && candidate.version === 2;
              const slots = custom
                ? "Custom"
                : candidate.allowances.concurrent_work === undefined
                  ? "Unlimited"
                  : `${formatCount(candidate.allowances.concurrent_work)} slots`;
              const projects =
                candidate.allowances.projects === undefined
                  ? "Unlimited"
                  : formatCount(candidate.allowances.projects);
              const label =
                current && candidate.id === "pilot_free"
                  ? "Legacy pilot"
                  : candidate.name || "Plan";
              return (
                <div
                  role="row"
                  key={`${candidate.id}-${candidate.version}`}
                  aria-current={current ? "true" : undefined}
                  className={cn(
                    "grid grid-cols-[1fr_auto] gap-2 p-3 md:grid-cols-1 md:grid-rows-[110px_44px_44px_44px_64px] md:gap-0 md:p-0",
                    current &&
                      "relative z-1 rounded-xl bg-info/8 ring-2 ring-info ring-inset",
                  )}
                >
                  <div
                    role="rowheader"
                    className="col-span-2 flex items-center justify-between gap-2 md:col-span-1 md:flex-col md:items-start md:justify-center md:px-3"
                  >
                    <h3 className="flex flex-wrap items-center gap-1.5 text-sm">
                      {label}
                      {current ? <Badge size="sm">Current</Badge> : null}
                    </h3>
                    <span
                      className={cn(
                        "tabular-nums",
                        custom
                          ? "text-xs text-muted-foreground"
                          : "text-lg font-semibold",
                      )}
                    >
                      {custom
                        ? "Contact us"
                        : formatUsd(
                            (candidate.monthly_usd_cents ?? 0) / 100,
                          ).replace(/\.00$/, "")}
                    </span>
                  </div>
                  <div className="col-span-2 flex flex-wrap gap-x-3 gap-y-1 md:contents">
                    <div
                      role="cell"
                      className="text-xs text-muted-foreground md:flex md:items-center md:border-t md:border-border/50 md:px-3 md:text-foreground"
                    >
                      <span className="sr-only">Concurrent work: </span>
                      {slots}
                      {custom ? (
                        <span className="md:hidden"> slots</span>
                      ) : null}
                    </div>
                    <div
                      role="cell"
                      className="text-xs md:flex md:items-center md:border-t md:border-border/50 md:px-3"
                    >
                      {candidate.features.includes("hosted_artifacts") ? (
                        <span className="inline-flex items-center gap-1 text-success-foreground">
                          <CheckIcon aria-hidden="true" className="size-3.5" />
                          <span className="md:sr-only">Hosted artifacts</span>
                          <span className="sr-only"> included</span>
                        </span>
                      ) : (
                        <span className="text-muted-foreground">
                          <span className="sr-only">
                            Hosted artifacts: not included
                          </span>
                          <span className="hidden md:inline">—</span>
                        </span>
                      )}
                    </div>
                    <div
                      role="cell"
                      className="text-xs text-muted-foreground md:flex md:items-center md:border-t md:border-border/50 md:px-3 md:text-foreground"
                    >
                      {projects}
                      <span className="md:sr-only"> projects</span>
                    </div>
                  </div>
                  <div
                    role="cell"
                    className="col-span-2 flex items-center text-xs text-muted-foreground md:col-span-1 md:border-t md:border-border/50 md:px-2"
                  >
                    {current
                      ? expiry
                      : moveButton(
                          candidate,
                          currentRef?.id === "free" &&
                            candidate.id === "starter",
                        )}
                  </div>
                </div>
              );
            })}
          </div>
          {canBill && usage.checkout_pending ? (
            <SettingsRow
              title="Checkout in progress"
              description="Finish the existing checkout, or wait for it to expire before choosing another plan."
            />
          ) : canBill && !usage.can_checkout && usage.can_manage ? (
            <SettingsRow
              title="Subscription changes"
              description="Manage your existing subscription through Manage billing."
            />
          ) : null}
          {checkout.error || portal.error ? (
            <SettingsRow title="Billing action failed">
              <ControlError
                message={
                  checkout.error?.message ?? portal.error?.message ?? null
                }
              />
            </SettingsRow>
          ) : null}
        </SettingsSection>
      )}
    </SettingsPageContainer>
  );
}

/** A plain sentence for each billing state the hub reports. */
export function billingStatusText(
  report: BillingReport,
  now: Date = new Date(),
): { title: string; description: string } {
  const state = report.state;
  const passed = (value: string) => value !== "" && !value.startsWith("0001-") && new Date(value).getTime() <= now.getTime();
  const plan = state.plan.id === "" ? report.entitlement.effective_base.id : state.plan.id;
  const until = (value: string) => (value.startsWith("0001-") || value === "" ? "" : new Date(value).toLocaleDateString());
  switch (state.status) {
    case "":
    case "free":
      if (report.entitlement.effective_base.id === "payment_pending") {
        return { title: "Payment pending", description: "Complete checkout to activate this organization's paid plan." };
      }
      return report.entitlement.source === "grant" || report.entitlement.effective_base.id !== "free"
        ? { title: "Complimentary access", description: `Plan ${plan}, granted by Detent.` }
        : { title: "Free plan", description: `Plan ${plan}. No payment details are on file.` };
    case "active":
      if (passed(state.access_until)) {
        return { title: "Access ended", description: "The paid period has ended. Renew or update payment in the billing portal." };
      }
      if (passed(state.paid_through)) {
        return {
          title: "Renewal pending",
          description: `Waiting for Stripe to confirm the renewal. Access continues until ${until(state.access_until)}.`,
        };
      }
      return { title: "Subscribed", description: `Plan ${plan} · paid through ${until(state.paid_through)}` };
    case "multiple_subscriptions":
      return { title: "Billing needs attention", description: "More than one subscription is active. Manage them in the billing portal or contact Detent support." };
    case "trialing":
      return { title: "Trial", description: `Plan ${plan} · trial access until ${until(state.access_until)}` };
    case "canceling":
      return { title: "Canceling", description: `Plan ${plan} ends ${until(state.access_until)}` };
    case "grace":
      return {
        title: "Payment needed",
        description: `The last payment failed. Access continues until ${until(state.grace_until)}; update the payment method in the billing portal.`,
      };
    case "payment_failed":
      return { title: "Payment failed", description: "Update the payment method in the billing portal. Existing work and exports stay available." };
    default:
      return { title: `Subscription ${state.status.replaceAll("_", " ")}`, description: `Plan ${plan}` };
  }
}

/** The notice after Checkout sends the owner back with `?checkout=returned`. */
export function billingReturnNotice(search: string): string | null {
  const params = new URLSearchParams(search);
  const checkout = params.get("checkout");
  if (checkout === "returned" || checkout === "success") {
    return "Back from checkout. Your plan changes as soon as Stripe confirms the payment.";
  }
  return null;
}

/** Statuses Stripe's confirmation of a Checkout can move billing into. */
const CONFIRMED_BILLING = new Set(["active", "trialing", "canceling", "multiple_subscriptions"]);

/**
 * After a Checkout return the payment may not be confirmed yet, so the report
 * is re-read a bounded number of times until Stripe's confirmation arrives.
 */
export function useBillingConfirmation(
  returned: boolean,
  billing: { readonly value: BillingReport | undefined; readonly loading: boolean; readonly refresh: () => Promise<void> },
  intervalMs = 3_000,
  attempts = 10,
): void {
  const tries = React.useRef(0);
  const { value, loading, refresh } = billing;
  React.useEffect(() => {
    if (!returned || loading || value === undefined) return;
    if (CONFIRMED_BILLING.has(value.state.status) || tries.current >= attempts) return;
    const timer = globalThis.setTimeout(() => {
      tries.current += 1;
      void refresh();
    }, intervalMs);
    return () => globalThis.clearTimeout(timer);
  }, [returned, loading, value, refresh, intervalMs, attempts]);
}

export function BillingSettings(): React.ReactElement {
  const api = useAccountApi();
  const billing = useResource<BillingReport>(() => api.billing(), [api]);
  const checkout = useMutation(async (price: string) => {
    const result = await api.checkout({ price, key: newKey() });
    globalThis.location?.assign(result.url);
    return result;
  });
  const portal = useMutation(async () => {
    const result = await api.portal({ key: newKey() });
    globalThis.location?.assign(result.url);
    return result;
  });
  const creationCheckout = React.useRef(false);
  React.useEffect(() => {
    const price = new URLSearchParams(globalThis.location?.search ?? "").get("checkout_price");
    if (
      creationCheckout.current || billing.value === undefined || !price ||
      !billing.value.can_checkout || !billing.value.prices.some((option) => option.id === price)
    ) return;
    creationCheckout.current = true;
    void checkout.call(price);
  }, [billing.value, checkout]);
  const notice = billingReturnNotice(globalThis.location?.search ?? "");
  const status = billing.value === undefined ? null : billingStatusText(billing.value);
  useBillingConfirmation(notice !== null, billing);

  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-billing"
        title="Billing"
        icon={<CreditCardIcon className="size-3.5" />}
      >
        {notice === null ? null : (
          <SettingsRow title="">
            <p role="status" className="pb-3 text-sm">
              {notice}
            </p>
          </SettingsRow>
        )}
        {billing.loading && billing.value === undefined ? (
          <SettingsRow title="Loading billing" />
        ) : billing.error !== null ? (
          <SettingsRow
            title="Billing is not available"
            description={
              billing.error.isAccessError
                ? "Billing needs owner access, and is closed during a support session."
                : billing.error.message
            }
          />
        ) : billing.value === undefined || status === null ? null : (
          <>
            <SettingsRow title={billing.value.entitlement.name ?? billing.value.entitlement.effective_base.id} status={planPriceText(billing.value.entitlement)} description="Detent subscriptions cover hosting. Model-provider charges remain separate; no AI-dollar allowance is included." />
            <SettingsRow title="Capacity" description="Work beyond the concurrent limit waits for a free slot; existing data, reading and exports remain available.">
              <AllowanceList rows={allowanceRows(billing.value.entitlement).filter((row) => row.name === "concurrent_work" || row.name === "unarchived_issues")} />
            </SettingsRow>
            <SettingsRow
              title={status.title}
              description={status.description}
              status={`Reconciled ${billing.value.reconciled_at || "never"}`}
              control={
                <Button
                  size="sm"
                  variant="outline"
                  disabled={portal.pending || billing.value.can_manage === false}
                  onClick={() => void portal.call()}
                >
                  {portal.pending ? "Opening…" : "Billing portal"}
                </Button>
              }
            />
            {billing.value.checkout_pending === true ? (
              <SettingsRow
                title="Checkout in progress"
                description="A checkout is still open. Finish it, or wait for it to expire before choosing another plan."
              />
            ) : null}
            {billing.value.prices.map((price) => (
              <SettingsRow
                key={price.id}
                title={price.label}
                description="Per organization; manage plan changes in the billing portal."
                control={
                  <Button
                    size="sm"
                    disabled={checkout.pending || billing.value?.can_checkout === false}
                    onClick={() => void checkout.call(price.id)}
                  >
                    {checkout.pending ? "Opening…" : "Upgrade"}
                  </Button>
                }
              />
            ))}
            {billing.value.chat_usage === undefined ? null : (
              <SettingsRow
                title="AI usage this billing period"
                description={`${billing.value.chat_usage.range.from.slice(0, 10)} – ${billing.value.chat_usage.range.to.slice(0, 10)} · ${billing.value.chat_usage.turns.toLocaleString()} ${billing.value.chat_usage.turns === 1 ? "turn" : "turns"}${billing.value.chat_usage.unpriced_turns > 0 ? ` · ${billing.value.chat_usage.unpriced_turns} ${billing.value.chat_usage.unpriced_turns === 1 ? "turn" : "turns"} awaiting pricing` : ""}`}
                status={`${billing.value.chat_usage.tokens.toLocaleString()} tokens · $${billing.value.chat_usage.cost_usd.toFixed(6)} USD`}
              />
            )}
            <SettingsRow title="">
              <div className="pb-3">
                <ControlError message={checkout.error?.message ?? portal.error?.message ?? null} />
              </div>
            </SettingsRow>
          </>
        )}
      </SettingsSection>
      {billing.value?.ai_credits === undefined ? null : (
        <CreditSettings credits={billing.value.ai_credits} refresh={billing.refresh} />
      )}
    </SettingsPageContainer>
  );
}



export function KeybindingsSettings(): React.ReactElement {
  const rows = React.useMemo(() => keybindingCatalogue(), []);
  return (
    <SettingsPageContainer>
      <SettingsSection
        id="settings-keybindings"
        title="Keybindings"
        icon={<KeyboardIcon className="size-3.5" />}
      >
        {SHELL_SHORTCUTS.map((binding) => (
          <SettingsRow
            key={binding.action}
            title={binding.action}
            control={
              <span className="flex items-center gap-1.5">
                {binding.keys.map((keys) => (
                  <Kbd key={keys}>{keys.replaceAll("+", " ")}</Kbd>
                ))}
              </span>
            }
          />
        ))}
        {rows.map((row) => (
          <SettingsRow
            key={row.command}
            title={row.label}
            data-testid={row.chord === null ? "keybinding-unbound" : "keybinding-bound"}
            data-command={row.command}
            {...(row.reason === null ? {} : { description: row.reason })}
            control={
              row.chord === null ? (
                <span className="text-muted-foreground text-xs" aria-disabled="true">
                  Unbound
                </span>
              ) : (
                <Kbd>{row.chord}</Kbd>
              )
            }
          />
        ))}
        <SettingsRow
          title="Rebinding"
          description="Keybindings are fixed in this release. Custom keybindings are not available yet."
        />
      </SettingsSection>
    </SettingsPageContainer>
  );
}

export function AboutSettings(): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  return (
    <SettingsPageContainer>
      <SettingsSection id="settings-about" title="About" icon={<InfoIcon className="size-3.5" />}>
        <SettingsRow
          title="Detent"
          description={bootstrap?.version ?? "This hub does not report a version."}
          status={bootstrap?.api_base}
        />
        <SettingsRow
          title="Interface"
          description="Portions derived from T3 Code. Copyright (c) 2026 T3 Tools Inc. MIT License."
        />
      </SettingsSection>
    </SettingsPageContainer>
  );
}

// --- The page ---------------------------------------------------------------

export function SettingsRoute({
  section = DEFAULT_SECTION,
  project = null,
  onNavigate,
}: {
  /** The path segment after `/settings`. */
  readonly section?: SettingsSectionId;
  /** `?project=`, for the Integrations section. */
  readonly project?: string | null;
  /** The router's navigation. Absent in a test that renders the page alone. */
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const canManage = bootstrap?.actor.can_manage ?? false;
  const supporting = bootstrap?.support != null;

  const items = React.useMemo(
    () => settingsNavItems({ canManage, supporting }),
    [canManage, supporting],
  );
  // A section the actor cannot read is not on the list, so asking for it by
  // path lands on the default rather than on an empty column.
  const requestedSection = section === "api-keys" && basePath().startsWith("/organizations/") ? "mcp" : section;
  React.useEffect(() => {
    if (requestedSection !== section) onNavigate?.("/settings/mcp");
  }, [onNavigate, requestedSection, section]);
  const activeId = items.some((item) => item.id === requestedSection && item.disabled !== true)
    ? requestedSection
    : DEFAULT_SECTION;
  const activeLabel = items.find((item) => item.id === activeId)?.label ?? SETTINGS_SECTION_LABELS[activeId];
  const projectName = bootstrap?.projects.find((candidate) => candidate.id === project)?.name;
  usePageTitle(
    activeId === "general" ? "Settings" : activeId === "runners" ? "Runners" : activeLabel,
    projectName,
  );

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-background text-foreground">
      <WorkspacePageHeader className="h-auto">
        <WorkspaceBreadcrumb ariaLabel="Settings breadcrumb" className="min-w-0 py-2">
          <WorkspaceBreadcrumbItem>
            <h1>Settings</h1>
          </WorkspaceBreadcrumbItem>
          <WorkspaceBreadcrumbSeparator />
          {activeId.startsWith("organization") ? (
            <>
              <WorkspaceBreadcrumbItem>Organization</WorkspaceBreadcrumbItem>
              <WorkspaceBreadcrumbSeparator />
            </>
          ) : null}
          <WorkspaceBreadcrumbItem current className="min-w-10">
            <span className="min-w-0 truncate">{activeId === "organization" ? "Overview" : activeLabel}</span>
          </WorkspaceBreadcrumbItem>
        </WorkspaceBreadcrumb>
      </WorkspacePageHeader>

      <div className="flex min-h-0 flex-1 flex-col">
        <SettingsBody section={activeId} project={project} onNavigate={onNavigate} />
      </div>
    </div>
  );
}

function SettingsBody({
  section,
  project,
  onNavigate,
}: {
  readonly section: SettingsSectionId;
  readonly project: string | null;
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  switch (section) {
    case "organization":
      return <OrganizationOverviewRoute />;
    case "organization/members":
      return <OrganizationMembersRoute />;
    case "organization/models":
      return <OrganizationModelsRoute />;
    case "projects":
      return <ProjectsSettings onNavigate={onNavigate} />;
    case "runners":
      return <RunnersSettings />;
    case "integrations":
      return <IntegrationsSettings project={project} onNavigate={onNavigate} />;
    case "mcp":
      return <MCPSettings />;
    case "plan":
      return <PlanSettings />;
    case "billing":
      return <BillingSettings />;
    case "keybindings":
      return <KeybindingsSettings />;
    case "whats-new":
      return <WhatsNewSettings />;
    case "about":
      return <AboutSettings />;
    case "general":
    default:
      return <GeneralSettings />;
  }
}

export { isSettingsSectionId, SETTINGS_SECTION_LABELS, settingsNavItems, WhatsNewSettings };
export type { SettingsSectionId };
