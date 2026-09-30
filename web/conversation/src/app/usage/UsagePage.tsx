import { CircleAlertIcon } from "lucide-react";
import React, { useMemo, useRef, useState } from "react";

import { Button } from "../../components/ui/button.tsx";
import { RefreshIcon } from "../../components/ui/refresh-icon.tsx";
import { ScrollArea } from "../../components/ui/scroll-area.tsx";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Toggle, ToggleGroup } from "../../components/ui/toggle-group.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import { cn } from "../../lib/utils.ts";
import { useAccountBootstrap } from "../account/context.ts";
import { useClient } from "../client.ts";
import {
  useUsage,
  type DailyTotals,
  type HourlyTotals,
  type MergedUsage,
  type UsageRange,
} from "./adapter.ts";
import {
  formatCount,
  formatDateTimeShort,
  formatDayShort,
  formatHourShort,
  formatPercent,
  formatTokens,
  formatUsd,
} from "./usageFormat.ts";
import { UsageLimitsSection } from "./UsageLimits.tsx";
import { UsageProviderChart, type UsageChartMetric } from "./UsageProviderChart.tsx";
import { PROVIDER_ORDER, presentationFor, providersWithUsage } from "./usageProviders.ts";
import {
  readUsagePagePreferences,
  saveUsagePagePreferences,
  type UsagePagePreferences,
} from "./usagePagePreferences.ts";

type UsageMetric = UsageChartMetric | "limits" | "runners";
const METRIC_OPTIONS = [
  { value: "cost", label: "Cost" },
  { value: "tokens", label: "Tokens" },
  { value: "limits", label: "Limits" },
  { value: "runners", label: "Runners" },
] as const satisfies readonly { value: UsageMetric; label: string }[];

function isUsageMetric(value: string | null | undefined): value is UsageMetric {
  return METRIC_OPTIONS.some((option) => option.value === value);
}

const WINDOW_OPTIONS = [
  { days: 1, label: "Past 24h", range: "24h" },
  { days: 7, label: "7 days", range: "7d" },
  { days: 30, label: "30 days", range: "30d" },
  { days: 90, label: "90 days", range: "90d" },
] as const satisfies readonly {
  days: UsagePagePreferences["windowDays"];
  label: string;
  range: UsageRange;
}[];

function isUsageWindowDays(value: number): value is UsagePagePreferences["windowDays"] {
  return WINDOW_OPTIONS.some((option) => option.days === value);
}

/** The hub range for a window the control offers. */
export function rangeForDays(days: number): UsageRange {
  return WINDOW_OPTIONS.find((option) => option.days === days)?.range ?? "30d";
}

// --- Route ------------------------------------------------------------------

/**
 * The route. It supplies the one thing `UsageView` cannot make for itself —
 * the read — and nothing else, so the page below stays renderable in a test
 * with plain props.
 */
export function UsageRoute(): React.ReactElement {
  const client = useClient();
  const bootstrap = useAccountBootstrap();
  const [preferences, setPreferences] = useState(readUsagePagePreferences);
  const range = rangeForDays(preferences.windowDays);
  const options = useMemo(
    () => ({ origin: client.http.origin, apiBase: client.http.apiBase }),
    [client],
  );
  const { merged, isPending, error, refresh } = useUsage(options, range);

  return (
    <UsageView
      merged={merged}
      isPending={isPending}
      errorMessage={error?.message ?? null}
      scope={bootstrap?.organization.name ?? "This organization"}
      preferences={preferences}
      onPreferencesChange={(next) => {
        setPreferences(next);
        saveUsagePagePreferences(next);
      }}
      onRefresh={refresh}
    />
  );
}

// --- View -------------------------------------------------------------------

export function UsageView({
  merged,
  isPending,
  errorMessage,
  scope,
  preferences,
  onPreferencesChange,
  onRefresh,
}: {
  readonly merged: MergedUsage;
  readonly isPending: boolean;
  readonly errorMessage: string | null;
  readonly scope: string;
  readonly preferences: UsagePagePreferences;
  readonly onPreferencesChange: (preferences: UsagePagePreferences) => void;
  readonly onRefresh: () => Promise<void>;
}): React.ReactElement {
  const metric = preferences.metric;
  const windowDays = preferences.windowDays;
  const showingLimits = metric === "limits";
  const showingRunners = metric === "runners";
  const chartMetric: UsageChartMetric = metric === "tokens" ? "tokens" : "cost";
  const [isRefreshing, setIsRefreshing] = useState(false);
  const refreshingRef = useRef(false);
  const [breakdown, setBreakdown] = useState<"model" | "time">("model");
  const isPast24Hours = windowDays === 1;

  const days = useMemo(() => merged.daily.map((entry) => entry.day), [merged.daily]);
  const hours = useMemo(() => merged.hourly.map((entry) => entry.hourStart), [merged.hourly]);
  // Newest first: the window can run 90 periods, so the interesting end
  // belongs at the top of the table.
  const breakdownPeriods = useMemo<readonly (DailyTotals | HourlyTotals)[]>(
    () => (isPast24Hours ? merged.hourly : merged.daily).toReversed(),
    [isPast24Hours, merged.daily, merged.hourly],
  );
  const breakdownModels = useMemo(
    () =>
      breakdown === "model" && metric === "tokens"
        ? merged.models.toSorted(
            (left, right) => right.totalTokens - left.totalTokens || right.costUsd - left.costUsd,
          )
        : merged.models,
    [breakdown, merged.models, metric],
  );
  const activeProviders = useMemo(() => providersWithUsage(merged.providers), [merged.providers]);
  const timeValueColumnWidth = `${60 / (activeProviders.length + 2)}%`;
  const providerLabels = useMemo(
    () => new Map(merged.providers.map((provider) => [provider.provider, provider.label])),
    [merged.providers],
  );
  const labelFor = (provider: string) =>
    presentationFor(provider, providerLabels.get(provider)).label;

  const selectWindow = (days: number) => {
    if (!isUsageWindowDays(days)) return;
    onPreferencesChange({ metric, windowDays: days });
  };
  const selectMetric = (nextMetric: UsageMetric) => {
    onPreferencesChange({ metric: nextMetric, windowDays });
  };
  const refreshWindow = () => {
    if (refreshingRef.current) return;
    refreshingRef.current = true;
    setIsRefreshing(true);
    void onRefresh().finally(() => {
      refreshingRef.current = false;
      setIsRefreshing(false);
    });
  };
  const windowLabel =
    merged.from === "" || merged.to === ""
      ? ""
      : isPast24Hours
        ? `${formatDateTimeShort(merged.from)} to ${formatDateTimeShort(merged.to)}`
        : `${formatDayShort(merged.from.slice(0, 10))} to ${formatDayShort(merged.to.slice(0, 10))}`;

  const topbarContent = (
    <div className="grid w-full min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-x-3 gap-y-2 py-2 xl:flex">
      <WorkspaceBreadcrumb ariaLabel="Usage breadcrumb" className="col-span-2 min-w-0">
        <WorkspaceBreadcrumbItem>
          <h1>Usage</h1>
        </WorkspaceBreadcrumbItem>
        <WorkspaceBreadcrumbSeparator />
        <WorkspaceBreadcrumbItem current className="min-w-10">
          <span className="min-w-0 truncate">{scope}</span>
        </WorkspaceBreadcrumbItem>
      </WorkspaceBreadcrumb>
      {!showingLimits ? (
        <span className="hidden min-w-0 truncate text-xs text-muted-foreground 2xl:block">
          {windowLabel}
        </span>
      ) : null}
      <div className="ms-auto hidden min-w-0 items-center justify-end gap-2 xl:flex">
        <ToggleGroup
          aria-label="Usage metric"
          variant="segmented"
          value={[metric]}
          onValueChange={(next) => {
            const value = next[0];
            if (isUsageMetric(value)) selectMetric(value);
          }}
        >
          {METRIC_OPTIONS.map((option) => (
            <Toggle key={option.value} value={option.value}>
              {option.label}
            </Toggle>
          ))}
        </ToggleGroup>
        {/* The period does not apply to Limits, so it stays in place but
            disabled; unmounting it shifted the metric toggle ~300px. */}
        <ToggleGroup
          aria-label="Usage period"
          variant="segmented"
          value={[String(windowDays)]}
          disabled={showingLimits}
          onValueChange={(next) => {
            const value = next[0];
            if (value) selectWindow(Number(value));
          }}
        >
          {WINDOW_OPTIONS.map((option) => (
            <Toggle key={option.days} value={String(option.days)}>
              {option.label}
            </Toggle>
          ))}
        </ToggleGroup>
        <Button
          onClick={refreshWindow}
          aria-label={showingLimits ? "Refresh limits" : "Refresh usage"}
          aria-busy={isRefreshing}
          disabled={isRefreshing}
          size="icon-sm"
          variant="ghost"
        >
          <RefreshIcon className="size-3.5" refreshing={isRefreshing} />
        </Button>
      </div>
      <div className="col-span-2 ms-auto flex min-w-0 items-center justify-end gap-1 xl:hidden">
        <Select
          value={metric}
          onValueChange={(value) => {
            if (isUsageMetric(value)) selectMetric(value);
          }}
        >
          <SelectTrigger
            aria-label="Usage metric"
            size="compact"
            variant="ghost"
            className="w-auto min-w-0"
          >
            <SelectValue>
              {METRIC_OPTIONS.find((option) => option.value === metric)?.label}
            </SelectValue>
          </SelectTrigger>
          <SelectPopup align="end" alignItemWithTrigger={false}>
            {METRIC_OPTIONS.map((option) => (
              <SelectItem key={option.value} value={option.value}>
                {option.label}
              </SelectItem>
            ))}
          </SelectPopup>
        </Select>
        <Select
          value={String(windowDays)}
          disabled={showingLimits}
          onValueChange={(value) => selectWindow(Number(value))}
        >
          <SelectTrigger
            aria-label="Usage period"
            size="compact"
            variant="ghost"
            className="w-auto min-w-0"
          >
            <SelectValue>
              {WINDOW_OPTIONS.find((option) => option.days === windowDays)?.label}
            </SelectValue>
          </SelectTrigger>
          <SelectPopup align="end" alignItemWithTrigger={false}>
            {WINDOW_OPTIONS.map((option) => (
              <SelectItem key={option.days} value={String(option.days)}>
                {option.label}
              </SelectItem>
            ))}
          </SelectPopup>
        </Select>
        <Button
          onClick={refreshWindow}
          aria-label={showingLimits ? "Refresh limits" : "Refresh usage"}
          aria-busy={isRefreshing}
          disabled={isRefreshing}
          size="icon-sm"
          variant="ghost"
        >
          <RefreshIcon className="size-3.5" refreshing={isRefreshing} />
        </Button>
      </div>
    </div>
  );

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-background text-foreground">
      <WorkspacePageHeader className="h-auto">{topbarContent}</WorkspacePageHeader>

      <ScrollArea className="min-h-0 flex-1">
        <WorkspacePageContainer width="wide">
          {errorMessage !== null ? (
            <p role="status" className="flex items-start gap-2 text-sm text-muted-foreground">
              <CircleAlertIcon className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden />
              {errorMessage}
            </p>
          ) : showingLimits ? (
            <UsageLimitsSection limits={merged.limits} />
          ) : isPending ? (
            <UsageSkeleton />
          ) : showingRunners ? (
            <RunnersSection merged={merged} />
          ) : (
            <>
              <section className="grid gap-6 lg:grid-cols-[minmax(0,18rem)_minmax(0,1fr)]">
                <div className="flex min-w-0 flex-col gap-5">
                  <div className="flex flex-col gap-1">
                    <span className="text-4xl font-semibold text-foreground tabular-nums">
                      {metric === "cost"
                        ? formatUsd(merged.costUsd)
                        : formatTokens(merged.totalTokens)}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      {metric === "cost"
                        ? `${formatCount(merged.sessions)} sessions · API estimate`
                        : `${formatCount(merged.sessions)} sessions`}
                    </span>
                  </div>

                  {activeProviders.map((provider) => {
                    const totals = merged.providers.find((entry) => entry.provider === provider);
                    const share =
                      metric === "cost" ? (totals?.costShare ?? 0) : (totals?.tokenShare ?? 0);
                    const providerSessions = totals?.sessions ?? 0;
                    const sessionLabel = `${formatCount(providerSessions)} ${
                      providerSessions === 1 ? "session" : "sessions"
                    }`;
                    return (
                      <div key={provider} className="flex flex-col gap-1">
                        <div className="flex items-baseline justify-between gap-4">
                          <span className="flex min-w-0 items-center gap-2 text-sm text-foreground">
                            <span
                              aria-hidden
                              className="size-2 shrink-0 rounded-full"
                              style={{
                                backgroundColor: presentationFor(provider).color,
                              }}
                            />
                            <ProviderMark provider={provider} className="size-4" />
                            <span className="flex min-w-0 items-baseline gap-1.5">
                              <span className="truncate">{labelFor(provider)}</span>
                              <span className="shrink-0 whitespace-nowrap text-[11px] text-muted-foreground tabular-nums">
                                {sessionLabel}
                              </span>
                            </span>
                          </span>
                          <span className="shrink-0 text-sm font-medium text-foreground tabular-nums">
                            {metric === "cost"
                              ? formatUsd(totals?.costUsd ?? 0)
                              : formatTokens(totals?.totalTokens ?? 0)}
                          </span>
                        </div>
                        <span className="text-xs text-muted-foreground">
                          {metric === "cost"
                            ? `${formatPercent(share)} of cost · ${formatTokens(totals?.totalTokens ?? 0)} tokens`
                            : `${formatPercent(share)} of tokens · ${formatUsd(totals?.costUsd ?? 0)}`}
                        </span>
                      </div>
                    );
                  })}
                </div>

                <div className="flex min-w-0 flex-col gap-3">
                  <h2 className="text-sm font-medium text-foreground">
                    {isPast24Hours ? "Hourly" : "Daily"}{" "}
                    {metric === "tokens" ? "processed tokens" : "cost"}
                  </h2>
                  <UsageProviderChart
                    providers={activeProviders}
                    days={days}
                    daily={merged.daily}
                    hours={hours}
                    hourly={merged.hourly}
                    metric={chartMetric}
                    referenceTime={merged.to === "" ? undefined : merged.to}
                    resolution={isPast24Hours ? "hour" : "day"}
                    timeZone={Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"}
                  />
                </div>
              </section>

              <section className="flex flex-col gap-2">
                <h2 className="text-sm font-medium text-foreground">Totals</h2>
                <div className="grid grid-cols-2 gap-x-6 gap-y-4 py-1 md:grid-cols-5">
                  <Metric label="Processed tokens" value={formatTokens(merged.totalTokens)} />
                  <Metric label="Cached input" value={formatTokens(merged.cachedInputTokens)} />
                  <Metric label="Uncached input" value={formatTokens(merged.uncachedInputTokens)} />
                  <Metric label="Output" value={formatTokens(merged.outputTokens)} />
                  <Metric
                    label="Cache savings"
                    value={formatUsd(merged.costQuality.cacheSavingsUsd)}
                  />
                </div>
              </section>

              <section className="flex flex-col gap-3">
                <div className="flex items-center justify-between gap-3">
                  <h2 className="text-sm font-medium text-foreground">Breakdown</h2>
                  <ToggleGroup
                    aria-label="Usage breakdown"
                    variant="segmented"
                    value={[breakdown]}
                    onValueChange={(next) => {
                      const value = next[0];
                      if (value === "model" || value === "time") setBreakdown(value);
                    }}
                  >
                    {(
                      [
                        { value: "model", label: "Model" },
                        { value: "time", label: isPast24Hours ? "Hour" : "Day" },
                      ] as const
                    ).map((option) => (
                      <Toggle key={option.value} value={option.value}>
                        {option.label}
                      </Toggle>
                    ))}
                  </ToggleGroup>
                </div>

                {breakdown === "model" ? (
                  <table className="w-full table-fixed text-sm">
                    <colgroup>
                      <col className="w-2/5" />
                      <col className="w-1/5" />
                      <col className="w-1/5" />
                      <col className="w-1/5" />
                    </colgroup>
                    <thead>
                      <tr className="border-b border-border text-left text-xs text-muted-foreground">
                        <th className="py-2 font-normal">Model</th>
                        <th className="py-2 text-right font-normal">Cost</th>
                        <th className="py-2 text-right font-normal">Share</th>
                        <th className="py-2 text-right font-normal">Tokens</th>
                      </tr>
                    </thead>
                    <tbody>
                      {breakdownModels.length === 0 ? (
                        <tr>
                          <td colSpan={4} className="py-6 text-center text-muted-foreground">
                            No activity in this window.
                          </td>
                        </tr>
                      ) : (
                        breakdownModels.map((model) => (
                          <tr
                            key={`${model.provider}:${model.model}`}
                            className="border-b border-border/50 transition-colors hover:bg-muted/50"
                          >
                            <td className="py-2 text-foreground">
                              <span className="flex items-center gap-2">
                                <ProviderMark provider={model.provider} className="size-3.5" />
                                {model.model}
                              </span>
                            </td>
                            <td className="py-2 text-right text-foreground tabular-nums">
                              {formatUsd(model.costUsd)}
                            </td>
                            <td className="py-2 text-right text-muted-foreground tabular-nums">
                              {formatPercent(model.costShare)}
                            </td>
                            <td className="py-2 text-right text-muted-foreground tabular-nums">
                              {formatTokens(model.totalTokens)}
                            </td>
                          </tr>
                        ))
                      )}
                    </tbody>
                  </table>
                ) : (
                  <table className="w-full table-fixed text-sm">
                    <colgroup>
                      <col className="w-2/5" />
                      {activeProviders.map((provider) => (
                        <col key={provider} style={{ width: timeValueColumnWidth }} />
                      ))}
                      <col style={{ width: timeValueColumnWidth }} />
                      <col style={{ width: timeValueColumnWidth }} />
                    </colgroup>
                    <thead>
                      <tr className="border-b border-border text-left text-xs text-muted-foreground">
                        <th className="py-2 font-normal">{isPast24Hours ? "Hour" : "Day"}</th>
                        {activeProviders.map((provider) => (
                          <th key={provider} className="py-2 text-right font-normal">
                            {labelFor(provider)}
                          </th>
                        ))}
                        <th className="py-2 text-right font-normal">Total</th>
                        <th className="py-2 text-right font-normal">Tokens</th>
                      </tr>
                    </thead>
                    <tbody>
                      {breakdownPeriods.length === 0 ? (
                        <tr>
                          <td
                            colSpan={activeProviders.length + 3}
                            className="py-6 text-center text-muted-foreground"
                          >
                            No activity in this window.
                          </td>
                        </tr>
                      ) : (
                        breakdownPeriods.map((period) => (
                          <tr
                            key={"hourStart" in period ? period.hourStart : period.day}
                            className="border-b border-border/50 transition-colors hover:bg-muted/50"
                          >
                            <td className="py-2 text-foreground">
                              {"hourStart" in period
                                ? formatHourShort(period.hourStart)
                                : formatDayShort(period.day)}
                            </td>
                            {activeProviders.map((provider) => (
                              <td
                                key={provider}
                                className="py-2 text-right text-muted-foreground tabular-nums"
                              >
                                {formatUsd(period.byProvider.get(provider)?.costUsd ?? 0)}
                              </td>
                            ))}
                            <td className="py-2 text-right text-foreground tabular-nums">
                              {formatUsd(period.costUsd)}
                            </td>
                            <td className="py-2 text-right text-muted-foreground tabular-nums">
                              {formatTokens(period.totalTokens)}
                            </td>
                          </tr>
                        ))
                      )}
                    </tbody>
                  </table>
                )}
              </section>
            </>
          )}
        </WorkspacePageContainer>
      </ScrollArea>
    </div>
  );
}

/** Brand mark for the harness a row belongs to. */
function ProviderMark({
  provider,
  className,
}: {
  readonly provider: string;
  readonly className: string;
}) {
  const Mark = presentationFor(provider).mark;
  return <Mark className={cn("shrink-0", className)} aria-hidden />;
}

function Metric({ label, value }: { readonly label: string; readonly value: string }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <span className="text-xs text-muted-foreground">{label}</span>
      <span className="text-base font-medium text-foreground tabular-nums">{value}</span>
    </div>
  );
}

/**
 * The Runners tab. §17.5 sends one row per runner for the window; this is the
 * Breakdown table with the fleet's own columns, so the two read as the same
 * table rather than two takes on one.
 */
export function RunnersSection({ merged }: { readonly merged: MergedUsage }): React.ReactElement {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between gap-3">
        <h2 className="text-sm font-medium text-foreground">Runners</h2>
        <span className="text-xs text-muted-foreground">
          {formatCount(merged.runners.length)} {merged.runners.length === 1 ? "runner" : "runners"}
        </span>
      </div>
      <table className="w-full table-fixed text-sm">
        <colgroup>
          <col className="w-2/6" />
          <col className="w-1/6" />
          <col className="w-1/6" />
          <col className="w-1/6" />
          <col className="w-1/6" />
        </colgroup>
        <thead>
          <tr className="border-b border-border text-left text-xs text-muted-foreground">
            <th className="py-2 font-normal">Runner</th>
            <th className="py-2 text-right font-normal">Busy</th>
            <th className="py-2 text-right font-normal">Sessions</th>
            <th className="py-2 text-right font-normal">Tokens</th>
            <th className="py-2 text-right font-normal">Cost</th>
          </tr>
        </thead>
        <tbody>
          {merged.runners.length === 0 ? (
            <tr>
              <td colSpan={5} className="py-6 text-center text-muted-foreground">
                No activity in this window.
              </td>
            </tr>
          ) : (
            merged.runners.map((runner) => (
              <tr
                key={runner.id}
                className="border-b border-border/50 transition-colors hover:bg-muted/50"
              >
                <td className="py-2 text-foreground">
                  <span className="flex min-w-0 flex-col">
                    <span className="truncate">{runner.displayName}</span>
                    <span className="truncate text-[11px] text-muted-foreground tabular-nums">
                      {formatPercent(runner.capacityUsed)} of capacity
                    </span>
                  </span>
                </td>
                <td className="py-2 text-right text-muted-foreground tabular-nums">
                  {formatBusy(runner.busySeconds)}
                </td>
                <td className="py-2 text-right text-muted-foreground tabular-nums">
                  {formatCount(runner.sessions)}
                </td>
                <td className="py-2 text-right text-muted-foreground tabular-nums">
                  {formatTokens(runner.totalTokens)}
                </td>
                <td className="py-2 text-right text-foreground tabular-nums">
                  {formatUsd(runner.costUsd)}
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
    </section>
  );
}

/** Seconds under lease, as hours where the window is long enough to need them. */
export function formatBusy(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3_600) return `${Math.round(seconds / 60)}m`;
  return `${formatCount(Math.round(seconds / 3_600))}h`;
}

/**
 * Stand-in with the loaded page's shape, using the shared `Skeleton` bars so it
 * breathes with the same `animate-skeleton` pulse as every other loading state.
 * Replaced by results as soon as the report arrives.
 */
function UsageSkeleton() {
  return (
    <>
      <section className="grid gap-6 lg:grid-cols-[minmax(0,18rem)_minmax(0,1fr)]">
        <div className="flex flex-col gap-5">
          <div className="flex flex-col gap-1">
            <Skeleton className="h-10 w-36" />
            <Skeleton className="h-4 w-32" />
          </div>
          {PROVIDER_ORDER.map((provider) => (
            <div key={provider} className="flex flex-col gap-1">
              <div className="flex min-h-5 items-center justify-between gap-4">
                <span className="flex items-center gap-2">
                  <Skeleton className="size-2 shrink-0 rounded-full" />
                  <Skeleton className="size-4 shrink-0 rounded-full" />
                  <Skeleton className="h-3.5 w-20" />
                </span>
                <Skeleton className="h-3.5 w-14" />
              </div>
              <Skeleton className="h-4 w-36" />
            </div>
          ))}
        </div>

        <div className="flex flex-col gap-3">
          <Skeleton className="h-5 w-24" />
          <div className="flex flex-col gap-1">
            <Skeleton className="ml-16 h-56 bg-muted-foreground/10" />
            <Skeleton className="ml-16 h-4 bg-muted-foreground/10" />
          </div>
        </div>
      </section>

      <section className="flex flex-col gap-2">
        <h2 className="text-sm font-medium text-foreground">Totals</h2>
        <div className="grid grid-cols-2 gap-x-6 gap-y-4 py-1 md:grid-cols-5">
          {["Processed tokens", "Cached input", "Uncached input", "Output", "Cache savings"].map(
            (label) => (
              <div key={label} className="flex flex-col gap-0.5">
                <span className="text-xs text-muted-foreground">{label}</span>
                <Skeleton className="h-6 w-16" />
              </div>
            ),
          )}
        </div>
      </section>

      <section className="flex flex-col gap-3">
        <div className="flex items-center justify-between gap-3">
          <h2 className="text-sm font-medium text-foreground">Breakdown</h2>
          <Skeleton className="h-7 w-28 rounded-lg" />
        </div>
        <Skeleton className="h-44 bg-muted-foreground/10" />
      </section>
    </>
  );
}
