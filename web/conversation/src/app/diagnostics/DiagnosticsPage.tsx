import React, { useEffect, useState } from "react";
import { Link } from "@tanstack/react-router";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { RefreshIcon } from "../../components/ui/refresh-icon.tsx";
import { ScrollArea } from "../../components/ui/scroll-area.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Toggle, ToggleGroup } from "../../components/ui/toggle-group.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import type { FleetRunner } from "../../contracts/account.ts";
import {
  findingDestination,
  findingsFromRead,
  orderedFindings,
  type DiagnosticsReport,
  type DiagnosticFinding,
} from "../../contracts/diagnostics.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { usePageTitle } from "../pageTitle.ts";
import { CapacityChart } from "./CapacityChart.tsx";

const WINDOWS = [
  { value: "24h", label: "Past 24h" },
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
] as const;
type Window = (typeof WINDOWS)[number]["value"];
const COVERAGE_SOURCES = [
  "Transitions",
  "Attempts",
  "Usage joins",
  "Activity receipts",
  "Queue intervals",
  "Skip reasons",
  "Merge identity",
  "Landing refusal reason",
  "Detector tick",
];
const CONFIGURATION = [
  "Plan gate",
  "Validator gate",
  "Gate kind",
  "Merge method",
  "Lifetime limit",
  "Runner capacity",
];
const unavailable = "Not recorded";
const count = (value: number | null | undefined) =>
  value == null ? unavailable : value.toLocaleString();
const duration = (seconds: number | null | undefined) =>
  seconds == null
    ? unavailable
    : seconds < 60
      ? `${seconds.toFixed(1)} s`
      : seconds < 3600
        ? `${(seconds / 60).toFixed(1)} min`
        : `${(seconds / 3600).toFixed(1)} h`;
const timestamp = (value: string) =>
  new Date(value).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });

export function DiagnosticsRoute(): React.ReactElement {
  usePageTitle("Diagnostics");
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const [range, setRange] = useState<Window>("24h");
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<{
    report: DiagnosticsReport | null;
    runners: readonly FleetRunner[] | null;
    findings: readonly DiagnosticFinding[] | null;
    detectorTick: string | null;
    pending: boolean;
    error: string | null;
  }>({
    report: null,
    runners: null,
    findings: null,
    detectorTick: null,
    pending: true,
    error: null,
  });
  useEffect(() => {
    let current = true;
    setState({
      report: null,
      runners: null,
      findings: null,
      detectorTick: null,
      pending: true,
      error: null,
    });
    void Promise.allSettled([
      api.diagnostics(range),
      api.fleet(),
      api.healthFindings(),
    ]).then(([report, fleet, health]) => {
      if (!current) return;
      setState({
        report: report.status === "fulfilled" ? report.value : null,
        runners: fleet.status === "fulfilled" ? fleet.value.runners : null,
        findings:
          health.status === "fulfilled" ? findingsFromRead(health.value) : null,
        detectorTick:
          health.status === "fulfilled" ? health.value.detector_tick : null,
        pending: false,
        error:
          report.status === "rejected"
            ? "Diagnostics are temporarily unavailable."
            : null,
      });
    });
    return () => {
      current = false;
    };
  }, [api, range, revision]);
  return (
    <DiagnosticsView
      {...state}
      scope={bootstrap?.organization.name ?? "This organization"}
      range={range}
      onRangeChange={setRange}
      onRefresh={() => setRevision((value) => value + 1)}
    />
  );
}

function Metric({
  label,
  value,
  detail,
}: {
  label: string;
  value: string;
  detail?: string;
}): React.ReactElement {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <span className="text-xl font-semibold tabular-nums">{value}</span>
      <span className="text-xs text-muted-foreground">{label}</span>
      {detail ? (
        <span className="text-xs text-muted-foreground">{detail}</span>
      ) : null}
    </div>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <section aria-label={title} className="flex min-w-0 flex-col gap-3">
      <h2 className="text-sm font-medium">{title}</h2>
      {children}
    </section>
  );
}

function Unavailable({
  children = "This source has not been recorded yet.",
}: {
  children?: React.ReactNode;
}): React.ReactElement {
  return (
    <p role="status" className="text-sm text-muted-foreground">
      {children}
    </p>
  );
}

export function DiagnosticsView({
  report,
  runners,
  findings: findingRows = report?.findings ?? null,
  detectorTick = report?.detector_tick ?? null,
  pending,
  error,
  scope,
  range,
  onRangeChange,
  onRefresh,
}: {
  readonly report: DiagnosticsReport | null;
  readonly runners: readonly FleetRunner[] | null;
  readonly findings?: readonly DiagnosticFinding[] | null;
  readonly detectorTick?: string | null;
  readonly pending: boolean;
  readonly error: string | null;
  readonly scope: string;
  readonly range: Window;
  readonly onRangeChange: (range: Window) => void;
  readonly onRefresh: () => void;
}): React.ReactElement {
  const findings = findingRows === null ? null : orderedFindings(findingRows);
  const used = runners?.reduce((sum, runner) => sum + runner.leases.length, 0);
  const capacity = runners?.reduce(
    (sum, runner) =>
      sum + Math.min(runner.capacity_limit, runner.reported_capacity),
    0,
  );
  const refusals = report?.refused;
  const totalRefused = refusals?.reduce((sum, row) => sum + row.count, 0);
  const conflictRefused =
    refusals == null
      ? undefined
      : refusals
          .filter((row) => row.reason === "conflict")
          .reduce((sum, row) => sum + row.count, 0);
  const conflictPercent =
    totalRefused != null &&
    totalRefused + (report?.merge_landed ?? 0) > 0 &&
    conflictRefused != null
      ? `${((100 * conflictRefused) / (totalRefused + (report?.merge_landed ?? 0))).toFixed(1)}%`
      : unavailable;
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-background text-foreground">
      <WorkspacePageHeader className="h-auto flex-wrap py-2">
        <WorkspaceBreadcrumb
          ariaLabel="Diagnostics breadcrumb"
          className="min-w-0"
        >
          <WorkspaceBreadcrumbItem>
            <h1>Diagnostics</h1>
          </WorkspaceBreadcrumbItem>
          <WorkspaceBreadcrumbSeparator />
          <WorkspaceBreadcrumbItem current>
            <span className="min-w-0 truncate">{scope}</span>
          </WorkspaceBreadcrumbItem>
        </WorkspaceBreadcrumb>
        <div className="ms-auto flex items-center gap-2">
          <ToggleGroup
            aria-label="Diagnostics period"
            variant="segmented"
            value={[range]}
            onValueChange={(values) => {
              const value = values[0];
              if (WINDOWS.some((window) => window.value === value))
                onRangeChange(value as Window);
            }}
          >
            {WINDOWS.map((window) => (
              <Toggle key={window.value} value={window.value}>
                {window.label}
              </Toggle>
            ))}
          </ToggleGroup>
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="Refresh diagnostics"
            disabled={pending}
            onClick={onRefresh}
          >
            <RefreshIcon refreshing={pending} className="size-3.5" />
          </Button>
        </div>
      </WorkspacePageHeader>
      <ScrollArea className="min-h-0 flex-1">
        <WorkspacePageContainer width="wide">
          {pending ? (
            <div
              aria-label="Loading diagnostics"
              className="flex flex-col gap-6"
            >
              <Skeleton className="h-20" />
              <Skeleton className="h-48" />
              <Skeleton className="h-48" />
            </div>
          ) : (
            <>
              {error ? <Unavailable>{error}</Unavailable> : null}
              {report ? (
                <p className="text-xs text-muted-foreground">
                  {timestamp(report.from)} to {timestamp(report.to)}
                  {report.partial
                    ? " · Partial coverage; recorded samples only"
                    : ""}
                </p>
              ) : null}
              <section
                aria-label="Diagnostics headline"
                className="grid grid-cols-2 gap-x-6 gap-y-4 md:grid-cols-5"
              >
                <Metric
                  label="Slots in use now"
                  value={
                    used == null || capacity == null
                      ? unavailable
                      : `${used} / ${capacity}`
                  }
                  detail={
                    report?.busy_percent == null
                      ? "Window busy % not recorded"
                      : `${report.busy_percent.toFixed(1)}% busy in window`
                  }
                />
                <Metric label="Open findings" value={count(findings?.length)} />
                <Metric
                  label="Stalled in lane"
                  value={count(report?.stalled)}
                />
                <Metric
                  label="Merge queue wait p50"
                  value={duration(report?.merge_wait_p50)}
                />
                <Metric
                  label="Landings refused on conflict"
                  value={conflictPercent}
                />
              </section>
              <Section title="Needs attention">
                {findings === null ? (
                  <Unavailable>
                    Findings are unavailable until the health detector records
                    them.
                  </Unavailable>
                ) : findings.length === 0 ? (
                  <Unavailable>No open findings.</Unavailable>
                ) : (
                  <div className="divide-y divide-border">
                    {findings.map((finding) => {
                      const destination = findingDestination(finding);
                      return (
                        <article
                          key={finding.id}
                          className="grid min-w-0 gap-2 py-3 sm:grid-cols-[8rem_minmax(0,1fr)_auto]"
                        >
                          <time
                            dateTime={finding.when}
                            className="text-xs text-muted-foreground"
                          >
                            {timestamp(finding.when)}
                          </time>
                          <div className="min-w-0 space-y-1">
                            {destination ? (
                              <Link
                                to={destination}
                                className="text-sm font-medium hover:underline"
                              >
                                {finding.summary}
                              </Link>
                            ) : (
                              <p className="text-sm font-medium">
                                {finding.summary}
                              </p>
                            )}
                            <p className="text-xs text-muted-foreground">
                              {finding.next_action}
                            </p>
                          </div>
                          <div className="flex items-start gap-1.5">
                            <Badge
                              variant={
                                finding.severity === "attention"
                                  ? "warning"
                                  : "info"
                              }
                            >
                              {finding.severity}
                            </Badge>
                            <Badge variant="secondary">{finding.class}</Badge>
                          </div>
                        </article>
                      );
                    })}
                  </div>
                )}
              </Section>
              <div className="grid min-w-0 gap-6 lg:grid-cols-2">
                <Section title="Capacity">
                  {report?.capacity && report.capacity.length > 0 ? (
                    <CapacityChart points={report.capacity} />
                  ) : (
                    <Unavailable>Hourly capacity is unavailable.</Unavailable>
                  )}
                  {report?.capacity?.some((point) => point.todo === null) ? (
                    <p className="text-xs text-muted-foreground">
                      Todo history has gaps in this window.
                    </p>
                  ) : null}
                  {runners === null ? (
                    <Unavailable>Runner capacity is unavailable.</Unavailable>
                  ) : runners.length === 0 ? (
                    <Unavailable>No runners enrolled.</Unavailable>
                  ) : (
                    runners.map((runner) => {
                      const limit = Math.min(
                        runner.capacity_limit,
                        runner.reported_capacity,
                      );
                      return (
                        <div
                          key={runner.id}
                          className="rounded-lg border border-border bg-card p-3"
                        >
                          <div className="flex flex-wrap items-center justify-between gap-2">
                            <Link
                              to="/fleet"
                              className="text-sm font-medium hover:underline"
                            >
                              {runner.display_name || runner.hostname}
                            </Link>
                            <Badge variant="secondary">{runner.health}</Badge>
                          </div>
                          <p className="mt-1 text-xs text-muted-foreground">
                            Limit {runner.capacity_limit} · Reported{" "}
                            {runner.reported_capacity} · {runner.leases.length}{" "}
                            in use
                          </p>
                          <div
                            aria-label={`${runner.leases.length} of ${limit} slots in use`}
                            className="mt-2 flex flex-wrap gap-1"
                          >
                            {Array.from(
                              {
                                length: Math.min(
                                  64,
                                  Math.max(limit, runner.leases.length),
                                ),
                              },
                              (_, index) => (
                                <span
                                  key={index}
                                  aria-hidden
                                  className={`size-3 rounded-xs ${index < runner.leases.length ? "bg-info-foreground" : "bg-muted"}`}
                                />
                              ),
                            )}
                            {limit > 64 ? (
                              <span className="text-xs text-muted-foreground">
                                +{limit - 64} slots
                              </span>
                            ) : null}
                          </div>
                        </div>
                      );
                    })
                  )}
                </Section>
                <div className="flex min-w-0 flex-col gap-6">
                  <Section title="Scheduler decisions">
                    <dl className="space-y-2 text-sm">
                      {[
                        ["Claimed", report?.claimed],
                        ["Ready", report?.ready],
                        ["Skipped", report?.skipped],
                      ].map(([label, value]) => (
                        <div
                          key={String(label)}
                          className="flex justify-between gap-4"
                        >
                          <dt>{label}</dt>
                          <dd className="text-muted-foreground tabular-nums">
                            {count(value as number | null | undefined)}
                          </dd>
                        </div>
                      ))}
                    </dl>
                    {report?.skip_reasons.map((row, index) => (
                      <div
                        key={`${row.source}-${row.reason}-${index}`}
                        className="flex items-start justify-between gap-4 border-t border-border pt-2 text-xs"
                      >
                        <span className="text-muted-foreground">
                          {row.reason}
                        </span>
                        <span className="tabular-nums">{count(row.count)}</span>
                      </div>
                    ))}
                  </Section>
                  <Section title="Merge queue">
                    <dl className="space-y-2 text-sm">
                      {[
                        ["Entered Merging", count(report?.merge_entered)],
                        [
                          "Wait p50 / p90 / max",
                          `${duration(report?.merge_wait_p50)} / ${duration(report?.merge_wait_p90)} / ${duration(report?.merge_wait_max)}`,
                        ],
                        ["Landed", count(report?.merge_landed)],
                      ].map(([label, value]) => (
                        <div
                          key={label}
                          className="flex flex-wrap justify-between gap-x-4 gap-y-1"
                        >
                          <dt>{label}</dt>
                          <dd className="text-muted-foreground tabular-nums">
                            {value}
                          </dd>
                        </div>
                      ))}
                    </dl>
                    {refusals == null ? (
                      <Unavailable>
                        Landing refusal reasons are not recorded.
                      </Unavailable>
                    ) : refusals.length === 0 ? (
                      <p className="text-xs text-muted-foreground">
                        No refused landings.
                      </p>
                    ) : (
                      refusals.map((row) => (
                        <div
                          key={row.reason}
                          className="flex justify-between gap-4 text-sm"
                        >
                          <span>Refused: {row.reason}</span>
                          <span className="tabular-nums">
                            {count(row.count)}
                          </span>
                        </div>
                      ))
                    )}
                  </Section>
                </div>
              </div>
              <Section title="Instrumentation coverage">
                <p className="text-xs text-muted-foreground">
                  Observed / total in this window. Unknown totals remain
                  unavailable.
                </p>
                <div className="divide-y divide-border">
                  {COVERAGE_SOURCES.map((source) => {
                    const row =
                      source === "Detector tick" && detectorTick
                        ? { source, observed: 1, total: 1 }
                        : report?.coverage.find(
                            (entry) => entry.source === source,
                          );
                    const measured =
                      row?.observed != null &&
                      row.total != null &&
                      row.total > 0 &&
                      row.observed <= row.total;
                    return (
                      <div
                        key={source}
                        className="grid min-w-0 grid-cols-[minmax(0,1fr)_3rem_7rem] items-center gap-3 py-2 text-xs sm:grid-cols-[minmax(0,1fr)_8rem_10rem]"
                      >
                        <span>{source}</span>
                        <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                          {measured ? (
                            <span
                              role="meter"
                              aria-label={`${source} coverage`}
                              aria-valuemin={0}
                              aria-valuemax={row.total!}
                              aria-valuenow={row.observed!}
                              className="block h-full bg-info-foreground"
                              style={{
                                width: `${(100 * row.observed!) / row.total!}%`,
                              }}
                            />
                          ) : null}
                        </div>
                        <span className="text-right text-muted-foreground tabular-nums">
                          {count(row?.observed)} / {count(row?.total)}
                        </span>
                      </div>
                    );
                  })}
                </div>
                <p className="text-xs text-muted-foreground">
                  Detector last tick:{" "}
                  {detectorTick ? timestamp(detectorTick) : unavailable}
                </p>
              </Section>
              <Section title="Configuration that shapes these numbers">
                <div className="divide-y divide-border">
                  {CONFIGURATION.map((name) => {
                    const row = report?.configuration?.find(
                      (entry) => entry.name === name,
                    );
                    const value =
                      name === "Runner capacity" && capacity != null
                        ? `${capacity} slots now`
                        : (row?.value ?? unavailable);
                    return (
                      <div
                        key={name}
                        className="grid min-w-0 gap-1 py-2 text-xs sm:grid-cols-[10rem_10rem_minmax(0,1fr)]"
                      >
                        <span>{name}</span>
                        <span className="tabular-nums">{value}</span>
                        <span className="text-muted-foreground">
                          {row?.effect ?? "Measured effect not recorded"}
                        </span>
                      </div>
                    );
                  })}
                </div>
              </Section>
            </>
          )}
        </WorkspacePageContainer>
      </ScrollArea>
    </div>
  );
}
