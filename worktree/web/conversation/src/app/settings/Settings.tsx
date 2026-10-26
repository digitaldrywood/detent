import { Link } from "@tanstack/react-router";
import { SHELL_SHORTCUTS } from "../lib/shortcuts.ts";
import {
  ArrowRightIcon,
  CheckIcon,
  ClockIcon,
  LockKeyholeIcon,
  TriangleAlertIcon,
  CreditCardIcon,
  InfoIcon,
  KeyboardIcon,
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
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import type { BillingReport, BillingUsageReport, ComparisonPlan, PlanReport } from "../../contracts/account.ts";
import { ControlError } from "../account/controls.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { Avatar } from "../../components/ui/avatar.tsx";
import { AccountWidgets } from "./AccountWidgets.tsx";
import { newKey } from "../account/idempotency.ts";
import { OrganizationMembersRoute } from "../account/Organization.tsx";
import { OrganizationOverviewRoute, OrganizationModelsRoute } from "../account/OrganizationOverview.tsx";
import { useMutation, useResource } from "../account/useResource.ts";
import { RunnersSettings } from "../fleet/RunnersSection.tsx";
import { usePageTitle } from "../pageTitle.ts";
import { keybindingCatalogue } from "../adapters/keybindings.ts";
import {
  DEFAULT_SECTION,
  isSettingsSectionId,
  SETTINGS_SECTION_LABELS,
  settingsNavItems,
  type SettingsSectionId,
} from "./sections.tsx";
import { BillingPlanStatus, CreditSettings } from "./CreditSettings.tsx";
import { approachingPlanLimits, nextFittingPlan, planUsageRows, planStatus } from "./planUsage.ts";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { SettingsHelp } from "./SettingsHelp.tsx";
import { cn } from "../../lib/utils.ts";
import { formatCount, formatUsd } from "../usage/usageFormat.ts";
import { SettingsPageContainer, SettingsRow, SettingsSection } from "./settingsLayout.tsx";
import { ProjectsSettings } from "./ProjectsSettings.tsx";
export { ProjectsSettings } from "./ProjectsSettings.tsx";
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

export function GeneralSettings(): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const api = useAccountApi();
  const selfHosted = bootstrap?.self_hosted ?? false;
  const notifications = useResource(() => selfHosted ? Promise.resolve({ enabled: false }) : api.issueNotificationPreference(), [api, selfHosted]);
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
  const actor = bootstrap?.actor;
  const name = [actor?.first_name, actor?.last_name].filter(Boolean).join(" ").trim() || actor?.email || "Your account";
  const role = actor?.role ? actor.role.charAt(0).toUpperCase() + actor.role.slice(1) : null;

  return (
    <SettingsPageContainer>
      <SettingsSection id="settings-general" title="General" icon={<Settings2Icon className="size-3.5" />}>
        <SettingsRow
          id="settings-general-account"
          title={<span className="flex items-center gap-3">
            <Avatar name={name} src={actor?.profile_picture_url} size="md" />
            <span className="min-w-0">
              <span className="flex flex-wrap items-center gap-2">{name}{role ? <Badge variant="secondary" size="sm">{role}</Badge> : null}</span>
              <span className="block break-all text-xs font-normal text-muted-foreground">{actor?.email}</span>
            </span>
          </span>}
          control={selfHosted ? undefined : <Button className="w-full sm:w-auto" size="sm" variant="outline" disabled={signOut.pending} onClick={() => void signOut.call()}>
            {signOut.pending ? "Signing out…" : "Sign out"}
          </Button>}
        />
        <ControlError message={signOut.error ? "Sign out failed. Please try again." : null} />
        {!selfHosted && <SettingsRow
          id="settings-general-issue-notifications"
          controlLayout="inline"
          title="Issue email notifications"
          description="Notify me about issues I filed or commented on."
          control={<Switch
            aria-label="Notify me about issues I filed or commented on"
            checked={notifications.value?.enabled ?? false}
            disabled={notifications.loading || notifications.error !== null || saveNotifications.pending}
            onCheckedChange={(enabled) => void saveNotifications.call(enabled)}
          />}
        />}
        <ControlError message={(notifications.error ?? saveNotifications.error)?.message ?? null} />
      </SettingsSection>
      {!selfHosted && <AccountWidgets />}
    </SettingsPageContainer>
  );
}

export function IntegrationsSettings(): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  return <SettingsPageContainer>
    <SlackSettings />
    {bootstrap ? <SettingsSection title="Organization Sprites"><SpritesCard projectId="" canManage={bootstrap.actor.can_manage} /></SettingsSection> : null}
  </SettingsPageContainer>;
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
            included_ai_credit_micros: 0,
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
              <span role="columnheader">AI credits / month</span>
              <span role="columnheader">Hosted artifacts</span>
              <span role="columnheader">Projects</span>
              <span role="columnheader">Storage</span>
              <span role="columnheader">Plan action or expiry</span>
            </div>
            <div
              aria-hidden="true"
              className="hidden grid-rows-[110px_44px_44px_44px_44px_44px_64px] text-xs text-muted-foreground md:grid"
            >
              <div />
              <div className="flex items-center border-t border-border/50 px-3">
                Concurrent work
              </div>
              <div className="flex items-center border-t border-border/50 px-3">
                AI credits / month
              </div>
              <div className="flex items-center border-t border-border/50 px-3">
                Hosted artifacts
              </div>
              <div className="flex items-center border-t border-border/50 px-3">
                Projects
              </div>
              <div className="flex items-center border-t border-border/50 px-3">
                Storage
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
                    "grid grid-cols-[1fr_auto] gap-2 p-3 md:grid-cols-1 md:grid-rows-[110px_44px_44px_44px_44px_44px_64px] md:gap-0 md:p-0",
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
                        custom || candidate.monthly_usd_cents == null
                          ? "text-xs text-muted-foreground"
                          : "text-lg font-semibold",
                      )}
                    >
                      {custom
                        ? "Contact us"
                        : candidate.monthly_usd_cents == null
                          ? "Custom"
                          : formatUsd(
                              candidate.monthly_usd_cents / 100,
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
                    <div role="cell" className="text-xs text-muted-foreground md:flex md:items-center md:border-t md:border-border/50 md:px-3 md:text-foreground">
                      <span className="md:sr-only">AI credits / month: </span>
                      {candidate.included_ai_credit_micros === null ? "Custom" : (candidate.included_ai_credit_micros ?? 0) > 0 ? formatUsd(candidate.included_ai_credit_micros! / 1_000_000) : "—"}
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
                    <div
                      role="cell"
                      className="text-xs text-muted-foreground md:flex md:items-center md:border-t md:border-border/50 md:px-3 md:text-foreground"
                    >
                      {custom
                        ? "Custom"
                        : candidate.allowances.collaboration_bytes === undefined
                          ? "Unlimited"
                          : formatUsageValue(candidate.allowances.collaboration_bytes, "bytes")}
                      <span className="md:sr-only"> storage</span>
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
            <div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-4 px-4 py-3 sm:flex sm:justify-between">
              <BillingPlanStatus report={billing.value} />
              <div className="contents sm:flex sm:items-center sm:gap-3">
                <Link to="/settings/$section" params={{ section: "plan" }} className="text-sm text-foreground hover:underline">Change plan →</Link>
                <Button size="sm" className="col-span-2 w-full sm:w-auto" variant="outline" disabled={portal.pending || billing.value.can_manage === false} onClick={() => void portal.call()}>{portal.pending ? "Opening…" : "Billing portal"}</Button>
              </div>
            </div>
            {checkout.error !== null || portal.error !== null ? <div className="px-4 py-3"><ControlError message={checkout.error?.message ?? portal.error?.message ?? null} /></div> : null}
          </>
        )}
      </SettingsSection>
      {billing.error !== null || billing.value?.ai_credits === undefined ? null : (
        <CreditSettings credits={billing.value.ai_credits} report={billing.value} refresh={billing.refresh} />
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
  runnerId,
  onNavigate,
}: {
  /** The path segment after `/settings`. */
  readonly section?: SettingsSectionId;
  readonly runnerId?: string;
  /** The router's navigation. Absent in a test that renders the page alone. */
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const canManage = bootstrap?.actor.can_manage ?? false;
  const supporting = bootstrap?.support != null;
  const selfHosted = bootstrap?.self_hosted ?? false;
  const [runnerName, setRunnerName] = React.useState("Runner");

  const items = React.useMemo(
    () => settingsNavItems({ canManage, supporting, selfHosted }),
    [canManage, supporting, selfHosted],
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
  usePageTitle(
    runnerId ? runnerName : activeId === "general" ? "Settings" : activeLabel,
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
          <WorkspaceBreadcrumbItem current={!runnerId} className="min-w-10">
            <span className="min-w-0 truncate">{runnerId ? <a href={`${basePath()}/settings/runners`} onClick={(event) => { if (onNavigate && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey) { event.preventDefault(); onNavigate("/settings/runners"); } }}>Runners</a> : activeId === "organization" ? "Overview" : activeLabel}</span>
          </WorkspaceBreadcrumbItem>
          {runnerId ? <><WorkspaceBreadcrumbSeparator /><WorkspaceBreadcrumbItem current><span className="truncate">{runnerName}</span></WorkspaceBreadcrumbItem></> : null}
        </WorkspaceBreadcrumb>
      </WorkspacePageHeader>

      <div className="flex min-h-0 flex-1 flex-col">
        <SettingsBody runnerId={runnerId} onRunnerName={setRunnerName} section={activeId} onNavigate={onNavigate} />
      </div>
    </div>
  );
}

function SettingsBody({
  runnerId, onRunnerName,
  section,
  onNavigate,
}: {
  readonly runnerId?: string;
  readonly onRunnerName?: (name: string) => void;
  readonly section: SettingsSectionId;
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
      return <RunnersSettings runnerId={runnerId} onNavigate={onNavigate} onRunnerName={onRunnerName} />;
    case "integrations":
      return <IntegrationsSettings />;
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
