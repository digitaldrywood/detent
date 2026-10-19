import { CapacityPanel } from "../fleet/CapacityPanel.tsx";
import { MetricTile } from "../../components/ui/metric-tile.tsx";
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
import type { AccountProject, FleetRunner, UpdatesReport } from "../../contracts/account.ts";
import {
  findingDestination,
  findingsFromRead,
  orderedFindings,
  type DiagnosticsReport,
  type DiagnosticFinding,
} from "../../contracts/diagnostics.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { getQueryData, scopeOf, setQueryData, useQuery } from "../lib/queryCache.ts";
import { DIAGNOSTICS_STALE_MS, diagnosticsKey, readDiagnostics } from "./diagnosticsQuery.ts";
import { usePageTitle } from "../pageTitle.ts";
import { useHealthFindings } from "./healthFindings.tsx";
import { CapacityChart } from "./CapacityChart.tsx";
import { FindingAskWorkspace } from "./FindingAsk.tsx";

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

interface FindingsState {
  readonly findings: readonly DiagnosticFinding[] | null;
  readonly detectorTick: string | null;
  readonly findingsError: string | null;
}

const EMPTY_FINDINGS: FindingsState = { findings: null, detectorTick: null, findingsError: null };
export function DiagnosticsRoute(): React.ReactElement {
  usePageTitle("Diagnostics");
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const sharedFindings = useHealthFindings();
  const [range, setRange] = useState<Window>("24h");
  const [askedFinding, setAskedFinding] = useState<DiagnosticFinding | null>(null);
  const [askFocusRequest, setAskFocusRequest] = useState(0);
  const projects = React.useMemo(() => (bootstrap?.projects ?? [])
    .filter((project) => project.profile === "native")
    .map((project) => project.id), [bootstrap?.projects]);
  const projectsKey = projects.join(",");
  const reportQuery = useQuery(diagnosticsKey(api, range), readDiagnostics(api, range),
    { staleTime: DIAGNOSTICS_STALE_MS, keepPreviousIn: String(scopeOf(api)) });
  const findingsKey = React.useMemo(() => ["diagnostics-findings", scopeOf(api), projectsKey] as const, [api, projectsKey]);
  const [findings, setFindings] = useState<{ readonly key: typeof findingsKey; readonly value: FindingsState }>(() =>
    ({ key: findingsKey, value: getQueryData<FindingsState>(findingsKey) ?? EMPTY_FINDINGS }));
  useEffect(() => {
    if (findings.key === findingsKey && findings.value !== EMPTY_FINDINGS) setQueryData(findingsKey, findings.value);
  }, [findings, findingsKey]);
  const findingsState = findings.key === findingsKey ? findings.value : getQueryData<FindingsState>(findingsKey) ?? EMPTY_FINDINGS;
  useEffect(() => {
    const read = sharedFindings.read;
    if (read === null && !sharedFindings.error) return;
    const nativeProjects = new Set(projects);
    const rows = (read === null ? [] : findingsFromRead(read)).filter((finding) => nativeProjects.has(finding.subject.project_id ?? ""));
    setFindings((previous) => {
      const prior = previous.key === findingsKey ? previous.value : EMPTY_FINDINGS;
      if (sharedFindings.error && read === null) return { key: findingsKey, value: { ...prior, findingsError: "Unable to refresh findings. Any findings below are from the last successful read." } };
      return { key: findingsKey, value: {
        findings: read?.last_tick_at == null ? null : rows.filter((finding) => finding.resolved_at == null),
        detectorTick: read?.last_tick_at ?? null,
        findingsError: sharedFindings.error ? "Unable to refresh findings. Any findings below are from the last successful read." : null,
      } };
    });
  }, [sharedFindings.read, sharedFindings.error, findingsKey, projects]);

  const state = {
    report: reportQuery.data?.report ?? null,
    runners: reportQuery.data?.runners ?? null,
    appUpdates: reportQuery.data?.appUpdates ?? null,
    ...findingsState,
    pending: reportQuery.pending,
    error: reportQuery.error !== null && reportQuery.data === undefined ? "Diagnostics are temporarily unavailable." : null,
  };
  const projectFor = (finding: DiagnosticFinding) =>
    bootstrap?.projects.find((project) => project.id === finding.subject.project_id);
  const askedProject = askedFinding === null ? undefined : projectFor(askedFinding);
  const view = (controls?: React.ReactNode) => (
    <DiagnosticsView
      {...state}
      scope={bootstrap?.organization.name ?? "This organization"}
      projects={bootstrap?.projects ?? []}
      canManage={bootstrap?.actor.can_manage ?? false}
      range={range}
      onRangeChange={setRange}
      onRefresh={() => {
        void reportQuery.refresh();
        sharedFindings.refresh();
      }}
      onAsk={(finding) => {
        setAskedFinding(finding);
        setAskFocusRequest((request) => request + 1);
      }}
      canAsk={(finding) => projectFor(finding)?.can_write !== false && projectFor(finding) !== undefined}
      panelControls={controls}
    />
  );
  return askedFinding === null || askedProject === undefined ? view() : (
    <FindingAskWorkspace key={`${askedProject.id}:${askedFinding.id}`} finding={askedFinding} project={askedProject} focusRequest={askFocusRequest}>
      {(controls) => view(controls)}
    </FindingAskWorkspace>
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
  appUpdates = null,
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
  onAsk,
  canAsk,
  panelControls,
}: {
  readonly projects?: readonly AccountProject[];
  readonly canManage?: boolean;
  readonly findingsError?: string | null;
  readonly report: DiagnosticsReport | null;
  readonly appUpdates?: UpdatesReport | null;
  readonly runners: readonly FleetRunner[] | null;
  readonly findings?: readonly DiagnosticFinding[] | null;
  readonly detectorTick?: string | null;
  readonly pending: boolean;
  readonly error: string | null;
  readonly scope: string;
  readonly range: Window;
  readonly onRangeChange: (range: Window) => void;
  readonly onRefresh: () => void;
  readonly onAsk?: (finding: DiagnosticFinding) => void;
  readonly canAsk?: (finding: DiagnosticFinding) => boolean;
  readonly panelControls?: React.ReactNode;
}): React.ReactElement {
  const findings = findingRows === null ? null : orderedFindings(findingRows.filter((finding) => finding.resolved_at == null));
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
          {panelControls}
          {appUpdates ? <p className="flex min-w-0 items-baseline gap-1.5 text-xs text-muted-foreground" title={`Hub ${appUpdates.current}`}>
            <span>Hub</span>
            <span className="max-w-28 truncate font-mono sm:max-w-none">{appUpdates.current}</span>
          </p> : null}
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
              {runners === null ? <Section title="Capacity right now"><Unavailable>Runner capacity is unavailable. Refresh to try again.</Unavailable></Section> : <CapacityPanel runners={runners} appUpdates={appUpdates} />}
              <Section title="Needs attention">
                {findingsError ? <Unavailable>{findingsError}</Unavailable> : null}
                {problems.length > 0 ? <div className="divide-y divide-border">
                  {problems.map((problem) => <article key={problem.id} className="grid min-w-0 gap-2 py-3 sm:grid-cols-[minmax(0,2.35fr)_minmax(0,1.3fr)]">
                    <div className="min-w-0 space-y-1 break-words">
                      <p className="flex flex-wrap gap-x-2 text-xs text-muted-foreground">
                        {problem.runners.map((runner) => <Link key={runner.id} to="/settings/runners" className="break-all hover:underline">{runner.display_name || runner.hostname}</Link>)}
                        <span className="break-all">· {problem.project}</span>
                      </p>
                      <p className="text-sm font-medium">{problem.summary}</p>
                      <p className="text-xs text-muted-foreground">{problem.impact}</p>
                    </div>
                    <div className="min-w-0">{problem.destination ? <Button variant="outline" size="sm" render={<Link to={problem.destination} />}>{problem.action}</Button> : <p className="text-xs text-muted-foreground">{problem.action}</p>}</div>
                  </article>)}
                </div> : null}
                {findings === null ? (
                  <Unavailable>
                    Findings are unavailable until the health detector records
                    them.
                  </Unavailable>
                ) : findings.length === 0 ? (
                  <p className="text-xs text-muted-foreground">{problems.length === 0 ? "No open findings." : "No additional open findings."} {detectorTick ? `Findings last checked ${timestamp(detectorTick)}.` : "Findings have not been checked yet."}</p>
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
                            <Button
                              variant="ghost"
                              size="xs"
                              disabled={canAsk?.(finding) === false}
                              title={canAsk?.(finding) === false ? "Project chat requires write access to the finding's project" : undefined}
                              aria-label={`Ask about ${finding.summary}`}
                              onClick={() => onAsk?.(finding)}
                            >
                              Ask
                            </Button>
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
              {findings === null || findings.length > 0 ? <p className="text-xs text-muted-foreground">{detectorTick ? `Findings last checked ${timestamp(detectorTick)}.` : "Findings have not been checked yet."}</p> : null}
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
                  <MetricTile size="compact" label="Open findings" value={count(findings == null ? null : findings.filter((finding) => finding.resolved_at == null).length + problems.length)} />
                  <MetricTile size="compact" label="Stalled in lane" value={count(report?.stalled)} />
                  <MetricTile size="compact" label="Merge queue wait p50" value={duration(report?.merge_wait_p50)} />
                  <MetricTile size="compact" label="Landings refused on conflict" value={conflictPercent} />
                </section>
                <Section title="Hourly capacity">
                  {report?.capacity && report.capacity.length > 0 ? <CapacityChart points={report.capacity} /> : <Unavailable>Hourly capacity is unavailable.</Unavailable>}
                  {report?.capacity?.some((point) => point.todo === null) ? <p className="text-xs text-muted-foreground">Todo history has gaps in this window.</p> : null}
                  <p className="text-xs text-muted-foreground">{report?.busy_percent == null ? "Window busy percentage not recorded." : `${report.busy_percent.toFixed(1)}% busy in this window.`}</p>
                </Section>
                <div className="grid min-w-0 gap-6 sm:grid-cols-2">
                  <Section title="Work throughput">
                    <div className="grid grid-cols-2 gap-4">
                      <MetricTile size="compact" label="Claimed" value={count(report?.claimed)} />
                      <MetricTile size="compact" label="Ready" value={count(report?.ready)} />
                      <MetricTile size="compact" label="Skipped" value={count(report?.skipped)} />
                    </div>
                  </Section>
                  <Section title="Merge throughput">
                    <div className="grid grid-cols-2 gap-4">
                      <MetricTile size="compact" label="Entered" value={count(report?.merge_entered)} />
                      <MetricTile size="compact" label="Landed" value={count(report?.merge_landed)} />
                      <MetricTile size="compact" label="Wait p90" value={duration(report?.merge_wait_p90)} />
                      <MetricTile size="compact" label="Longest wait" value={duration(report?.merge_wait_max)} />
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
