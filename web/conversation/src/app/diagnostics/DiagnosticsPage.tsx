import { CapacityPanel } from "../fleet/CapacityPanel.tsx";
import { runnerFindings } from "./runnerFindings.ts";
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
import type { AccountProject, FleetRunner } from "../../contracts/account.ts";
import {
  findingDestination,
  findingsFromRead,
  orderedFindings,
  type DiagnosticsReport,
  type DiagnosticFinding,
} from "../../contracts/diagnostics.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { usePageTitle } from "../pageTitle.ts";
import { subscribeProjectEvents } from "../work/lib/projectEvents.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import { CapacityChart } from "./CapacityChart.tsx";

const WINDOWS = [
  { value: "24h", label: "Past 24h" },
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
] as const;
type Window = (typeof WINDOWS)[number]["value"];
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
  const http = useWorkHttp();
  const [range, setRange] = useState<Window>("24h");
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<{
    report: DiagnosticsReport | null;
    runners: readonly FleetRunner[] | null;
    findings: readonly DiagnosticFinding[] | null;
    detectorTick: string | null;
    findingsError: string | null;
    pending: boolean;
    error: string | null;
  }>({
    report: null,
    runners: null,
    findings: null,
    detectorTick: null,
    findingsError: null,
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
      findingsError: null,
      pending: true,
      error: null,
    });
    const projects = (bootstrap?.projects ?? [])
      .filter((project) => project.profile === "native")
      .map((project) => project.id);
    let request = 0;
    let tick: string | null = null;
    let since: string | undefined;
    const refreshFindings = async (live: boolean) => {
      const version = ++request;
      try {
        const [open, resolved] = await Promise.all([
          api.healthFindings(projects),
          live ? api.healthFindings(projects, { state: "resolved", since }) : null,
        ]);
        if (!current || request !== version) return;
        since ??= open.last_tick_at ?? undefined;
        tick = open.last_tick_at;
        setState((previous) => {
          const findings =
            open.last_tick_at === null ? null : findingsFromRead(open);
          const updates = new Map(
            [
              ...(resolved === null ? [] : findingsFromRead(resolved)),
              ...(findings ?? []),
            ].map((finding) => [finding.id, finding]),
          );
          const retained = live
            ? (previous.findings ?? []).map(
                (finding) => updates.get(finding.id) ?? finding,
              )
            : [];
          const ids = new Set(retained.map((finding) => finding.id));
          return {
            ...previous,
            findings:
              findings === null
                ? null
                : [
                    ...retained,
                    ...findings.filter((finding) => !ids.has(finding.id)),
                  ],
            detectorTick: open.last_tick_at,
            findingsError: null,
          };
        });
      } catch {
        if (current && request === version) setState((previous) => ({ ...previous, findingsError: "Unable to refresh findings. Any findings below are from the last successful read." }));
        return;
      }
    };
    void Promise.allSettled([
      api.diagnostics(range),
      api.fleet(),
      refreshFindings(false),
    ]).then(([report, fleet]) => {
      if (!current) return;
      setState((previous) => ({
        ...previous,
        report: report.status === "fulfilled" ? report.value : null,
        runners: fleet.status === "fulfilled" ? fleet.value.runners : null,
        pending: false,
        error:
          report.status === "rejected"
            ? "Diagnostics are temporarily unavailable."
            : null,
      }));
    });
    const refreshLive = () => {
      if (document.visibilityState === "visible") void refreshFindings(true);
    };
    const unsubscribe =
      projects[0] === undefined
        ? undefined
        : subscribeProjectEvents(http, projects[0], {
            "health.findings": ((event: MessageEvent<string>) => {
              if (tick === event.data) return;
              refreshLive();
            }) as EventListener,
            open: refreshLive,
          });
    document.addEventListener("visibilitychange", refreshLive);
    return () => {
      current = false;
      unsubscribe?.();
      document.removeEventListener("visibilitychange", refreshLive);
    };
  }, [api, http, bootstrap?.projects, range, revision]);
  return (
    <DiagnosticsView
      {...state}
      scope={bootstrap?.organization.name ?? "This organization"}
      projects={bootstrap?.projects ?? []}
      canManage={bootstrap?.actor.can_manage ?? false}
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
  projects = [],
  canManage = false,
  findingsError = null,
}: {
  readonly projects?: readonly AccountProject[];
  readonly canManage?: boolean;
  readonly findingsError?: string | null;
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
  const problems = runnerFindings(runners ?? [], projects, canManage);
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
              {runners === null ? <Section title="Capacity right now"><Unavailable>Runner capacity is unavailable. Refresh to try again.</Unavailable></Section> : <CapacityPanel runners={runners} />}
              <Section title="Needs attention">
                {findingsError ? <Unavailable>{findingsError}</Unavailable> : null}
                {problems.length > 0 ? <div className="divide-y divide-border">
                  {problems.map((problem) => <article key={problem.id} className="grid min-w-0 gap-2 py-3 sm:grid-cols-[minmax(0,1fr)_auto]">
                    <div className="min-w-0 space-y-1 break-words">
                      <p className="flex flex-wrap gap-x-2 text-xs text-muted-foreground">
                        {problem.runners.map((runner) => <Link key={runner.id} to="/settings/runners" className="break-all hover:underline">{runner.display_name || runner.hostname}</Link>)}
                        <span className="break-all">· {problem.project}</span>
                      </p>
                      <p className="text-sm font-medium">{problem.summary}</p>
                      <p className="text-xs text-muted-foreground">{problem.impact}</p>
                      {!problem.destination ? <p className="text-xs text-muted-foreground">{problem.action}</p> : null}
                    </div>
                    {problem.destination ? <Button variant="outline" size="sm" render={<Link to={problem.destination} />}>{problem.action}</Button> : null}
                  </article>)}
                </div> : null}
                {findings === null ? (
                  <Unavailable>
                    Findings are unavailable until the health detector records
                    them.
                  </Unavailable>
                ) : findings.length === 0 ? (
                  <Unavailable>{problems.length === 0 ? "No open findings." : "No additional open findings."}</Unavailable>
                ) : (
                  <div className="divide-y divide-border">
                    {findings.map((finding) => {
                      const destination = findingDestination(finding);
                      return (
                        <article
                          key={finding.id}
                          className={`grid min-w-0 gap-2 py-3 transition-opacity motion-reduce:transition-none sm:grid-cols-[8rem_minmax(0,1fr)_auto] ${finding.resolved_at ? "opacity-60" : "opacity-100"}`}
                        >
                          <time
                            dateTime={finding.resolved_at ?? finding.when}
                            className="text-xs text-muted-foreground"
                          >
                            {timestamp(finding.resolved_at ?? finding.when)}
                          </time>
                          <div className="min-w-0 space-y-1 break-words">
                            <p className="text-xs text-muted-foreground break-all">{finding.subject.runner_id
                              ? runners?.find((runner) => runner.id === finding.subject.runner_id)?.display_name || "Runner unavailable to you"
                              : finding.subject.kind === "fleet" ? "Organization runners" : projects.find((project) => project.id === finding.subject.project_id)?.name ?? "Project unavailable to you"}
                              {finding.subject.runner_id ? ` · ${projects.find((project) => project.id === finding.subject.project_id)?.name ?? "Assigned projects"}` : null}</p>
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
                                finding.resolved_at
                                  ? "secondary"
                                  : finding.severity === "attention"
                                    ? "warning"
                                    : "info"
                              }
                            >
                              {finding.resolved_at ? "Resolved" : finding.severity}
                            </Badge>
                            <Badge variant="secondary">{finding.class}</Badge>
                          </div>
                        </article>
                      );
                    })}
                  </div>
                )}
              </Section>
              <p className="text-xs text-muted-foreground">{detectorTick ? `Findings last checked ${timestamp(detectorTick)}.` : "Findings have not been checked yet."}</p>
              <section aria-label="Capacity and throughput history" className="flex min-w-0 flex-col gap-6 border-t border-border pt-6">
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <h2 className="text-sm font-medium">Capacity and throughput history</h2>
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
                </div>
                {error ? <Unavailable>{error}</Unavailable> : null}
                {report ? <p className="text-xs text-muted-foreground">{timestamp(report.from)} to {timestamp(report.to)}{report.partial ? " · Partial history; recorded samples only" : ""}</p> : null}
                <section aria-label="Diagnostics headline" className="grid grid-cols-2 gap-x-6 gap-y-4 md:grid-cols-4">
                  <Metric label="Open findings" value={count(findings == null ? null : findings.filter((finding) => finding.resolved_at == null).length + problems.length)} />
                  <Metric label="Stalled in lane" value={count(report?.stalled)} />
                  <Metric label="Merge queue wait p50" value={duration(report?.merge_wait_p50)} />
                  <Metric label="Landings refused on conflict" value={conflictPercent} />
                </section>
                <Section title="Hourly capacity">
                  {report?.capacity && report.capacity.length > 0 ? <CapacityChart points={report.capacity} /> : <Unavailable>Hourly capacity is unavailable.</Unavailable>}
                  {report?.capacity?.some((point) => point.todo === null) ? <p className="text-xs text-muted-foreground">Todo history has gaps in this window.</p> : null}
                  <p className="text-xs text-muted-foreground">{report?.busy_percent == null ? "Window busy percentage not recorded." : `${report.busy_percent.toFixed(1)}% busy in this window.`}</p>
                </Section>
                <div className="grid min-w-0 gap-6 sm:grid-cols-2">
                  <Section title="Work throughput">
                    <div className="grid grid-cols-2 gap-4">
                      <Metric label="Claimed" value={count(report?.claimed)} />
                      <Metric label="Ready" value={count(report?.ready)} />
                      <Metric label="Skipped" value={count(report?.skipped)} />
                    </div>
                  </Section>
                  <Section title="Merge throughput">
                    <div className="grid grid-cols-2 gap-4">
                      <Metric label="Entered" value={count(report?.merge_entered)} />
                      <Metric label="Landed" value={count(report?.merge_landed)} />
                      <Metric label="Wait p90" value={duration(report?.merge_wait_p90)} />
                      <Metric label="Longest wait" value={duration(report?.merge_wait_max)} />
                    </div>
                  </Section>
                </div>
              </section>
            </>
          )}
        </WorkspacePageContainer>
      </ScrollArea>
    </div>
  );
}
