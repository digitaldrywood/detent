import React, { useEffect, useState } from "react";
import { Link } from "@tanstack/react-router";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { RefreshIcon } from "../../components/ui/refresh-icon.tsx";
import { ScrollArea } from "../../components/ui/scroll-area.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Toggle, ToggleGroup } from "../../components/ui/toggle-group.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import type {
  ReportsReport,
  ReportsRange,
  ReportsTimeView,
} from "../../contracts/reports.ts";
import { useAccountApi } from "../account/context.ts";
import { useSidebarData } from "../adapters/sidebarData.tsx";
import { usePageTitle } from "../pageTitle.ts";
import { formatCount, formatTokens, formatUsd } from "../usage/usageFormat.ts";

import { reworkByCause } from "./rework.ts";

const WINDOWS: readonly { value: ReportsRange; label: string }[] = [
  { value: "24h", label: "Past 24h" },
  { value: "48h", label: "48h" },
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
];
const UNAVAILABLE = "Not recorded";
const LANES: Readonly<Record<string, string>> = {
  Todo: "var(--info)",
  "In Progress": "var(--success)",
  Merging: "var(--foreground)",
  Rework: "var(--warning)",
  Blocked: "var(--muted-foreground)",
  "Human Review":
    "color-mix(in oklab, var(--muted-foreground) 60%, var(--background))",
};
const laneColor = (lane: string) => LANES[lane] ?? "var(--muted-foreground)";
const hours = (seconds: number) =>
  seconds < 3600
    ? `${(seconds / 60).toFixed(1)} min`
    : `${(seconds / 3600).toFixed(1)} h`;
const compactDuration = (seconds: number) =>
  seconds < 3600
    ? `${(seconds / 60).toFixed(0)}m`
    : `${(seconds / 3600).toFixed(1)}h`;
const percent = (value: number | null) =>
  value === null ? UNAVAILABLE : `${value.toFixed(1)}%`;
const at = (value: string) =>
  new Date(value).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });

export function ReportsRoute(): React.ReactElement {
  usePageTitle("Reports");
  const api = useAccountApi();
  const sidebar = useSidebarData();
  const project = sidebar?.projects.find(
    (p) => p.id === sidebar.activeProjectId,
  );
  const [range, setRange] = useState<ReportsRange>("48h");
  const [view, setView] = useState<ReportsTimeView>("system");
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<{
    report: ReportsReport | null;
    pending: boolean;
    error: string | null;
    project: string | null;
    range: ReportsRange;
  }>({ report: null, pending: true, error: null, project: null, range });
  useEffect(() => {
    let current = true;
    setState({
      report: null,
      pending: project !== undefined,
      error: null,
      project: project?.id ?? null,
      range,
    });
    if (project !== undefined)
      void api.reports(project.id, range).then(
        (report) => {
          if (current)
            setState({
              report,
              pending: false,
              error: null,
              project: project.id,
              range,
            });
        },
        () => {
          if (current)
            setState({
              report: null,
              pending: false,
              error: "Reports are temporarily unavailable.",
              project: project.id,
              range,
            });
        },
      );
    return () => {
      current = false;
    };
  }, [api, project?.id, range, revision]);
  const current =
    state.project === (project?.id ?? null) && state.range === range;
  return (
    <ReportsView
      report={current ? state.report : null}
      pending={project !== undefined && (!current || state.pending)}
      error={current ? state.error : null}
      scope={project?.name ?? "Choose a project"}
      range={range}
      view={view}
      onRangeChange={setRange}
      onViewChange={setView}
      onRefresh={() => setRevision((v) => v + 1)}
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
      <span className="text-2xl font-semibold tabular-nums">{value}</span>
      <span className="text-xs text-muted-foreground">{label}</span>
      {detail && (
        <span className="text-xs text-muted-foreground">{detail}</span>
      )}
    </div>
  );
}
function Section({
  title,
  partial = false,
  children,
}: {
  title: string;
  partial?: boolean;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <section
      aria-label={title}
      className="flex min-w-0 flex-col gap-3 border-t border-border pt-5"
    >
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-sm font-medium">{title}</h2>
        {partial && (
          <Badge variant="warning" size="sm">
            Partial
          </Badge>
        )}
      </div>
      {children}
    </section>
  );
}
function Missing({
  children = UNAVAILABLE,
}: {
  children?: React.ReactNode;
}): React.ReactElement {
  return <p className="text-xs text-muted-foreground">{children}</p>;
}
function IssueLink({
  id,
  titles,
}: {
  id: string;
  titles: ReportsReport["titles"];
}): React.ReactElement {
  return (
    <Link
      to="/work/i/$workItemId"
      params={{ workItemId: id }}
      search={{ tab: "diagnostics" } as never}
      className="break-words text-foreground underline underline-offset-2"
    >
      {titles[id] ?? id}
    </Link>
  );
}

export function ReportsView({
  report,
  pending,
  error,
  scope,
  range,
  view,
  onRangeChange,
  onViewChange,
  onRefresh,
}: {
  report: ReportsReport | null;
  pending: boolean;
  error: string | null;
  scope: string;
  range: ReportsRange;
  view: ReportsTimeView;
  onRangeChange: (range: ReportsRange) => void;
  onViewChange: (view: ReportsTimeView) => void;
  onRefresh: () => void;
}): React.ReactElement {
  const p = report?.analytics;
  const unavailable = (key: string) =>
    report?.unavailable.includes(key) ?? true;
  const residenceMissing = unavailable(
    "lane_residence_and_unclaimed_waits_unavailable",
  );
  const completion = report?.completion;
  const timing = view === "system" ? completion?.system : completion?.lead;
  const lanes =
    (view === "lead"
      ? report?.completion.lanes
      : p?.lane_residence.lanes
    )?.filter(
      (lane) =>
        lane.group !== "excluded" &&
        lane.lane !== "Backlog" &&
        lane.lane !== "Triage",
    ) ?? [];
  const delta =
    report &&
    !residenceMissing &&
    !report.previous.partial &&
    !report.completion.partial
      ? report.completion.done - report.previous.done
      : null;
  const usageMissing = unavailable("recorded_usage");
  const completionMissing = residenceMissing || timing?.count === 0;
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-background text-foreground">
      <WorkspacePageHeader className="h-auto">
        <div className="flex w-full min-w-0 flex-wrap items-center gap-2 py-2">
          <WorkspaceBreadcrumb
            ariaLabel="Reports breadcrumb"
            className="min-w-0 flex-1"
          >
            <WorkspaceBreadcrumbItem>
              <h1>Reports</h1>
            </WorkspaceBreadcrumbItem>
            <WorkspaceBreadcrumbSeparator />
            <WorkspaceBreadcrumbItem current>
              <span className="truncate">{scope}</span>
            </WorkspaceBreadcrumbItem>
          </WorkspaceBreadcrumb>
          <Button
            size="icon-sm"
            variant="ghost"
            aria-label="Refresh reports"
            disabled={pending}
            onClick={onRefresh}
          >
            <RefreshIcon refreshing={pending} />
          </Button>
          <div className="flex w-full flex-wrap gap-2 xl:w-auto">
            <ToggleGroup
              aria-label="Time view"
              variant="segmented"
              value={[view]}
              onValueChange={(v) => {
                if (v[0] === "system" || v[0] === "lead") onViewChange(v[0]);
              }}
            >
              <Toggle value="system">System time</Toggle>
              <Toggle value="lead">Lead time</Toggle>
            </ToggleGroup>
            <ToggleGroup
              aria-label="Report window"
              variant="segmented"
              value={[range]}
              onValueChange={(v) => {
                const match = WINDOWS.find((w) => w.value === v[0]);
                if (match) onRangeChange(match.value);
              }}
            >
              {WINDOWS.map((w) => (
                <Toggle key={w.value} value={w.value}>
                  {w.label}
                </Toggle>
              ))}
            </ToggleGroup>
          </div>
        </div>
      </WorkspacePageHeader>
      <ScrollArea className="min-h-0 flex-1">
        <WorkspacePageContainer width="wide" data-testid="reports-page">
          {pending ? (
            <div
              role="status"
              aria-label="Loading reports"
              className="flex flex-col gap-6"
            >
              <Skeleton className="h-20 w-full" />
              <Skeleton className="h-44 w-full" />
              <Skeleton className="h-44 w-full" />
            </div>
          ) : error ? (
            <p role="status" className="text-sm text-muted-foreground">
              {error}
            </p>
          ) : !report || !p ? (
            <Missing>
              Choose a project in the sidebar to view its reports.
            </Missing>
          ) : (
            <>
              <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                <span>
                  {at(p.window.from)} to {at(p.window.to)} · complete hours
                </span>
                {(p.partial || completion?.partial) && (
                  <Badge variant="warning" size="sm">
                    Partial
                  </Badge>
                )}
              </div>
              <section
                aria-label="Headline metrics"
                className="grid grid-cols-2 gap-x-6 gap-y-5 lg:grid-cols-6"
              >
                <Metric
                  label="Issues done"
                  value={
                    residenceMissing
                      ? UNAVAILABLE
                      : formatCount(report.completion.done)
                  }
                  detail={
                    delta === null
                      ? "Prior window unavailable"
                      : `${delta >= 0 ? "+" : ""}${delta} vs prior window`
                  }
                />
                <Metric
                  label={`${view === "system" ? "System" : "Lead"} time / issue p50`}
                  value={
                    completionMissing || !timing
                      ? UNAVAILABLE
                      : hours(timing.p50_seconds)
                  }
                  detail={
                    view === "system"
                      ? "First Todo to Done · controlled lanes"
                      : "Created to Done · includes held time"
                  }
                />
                <Metric
                  label={`${view === "system" ? "System" : "Lead"} time / issue p90`}
                  value={
                    completionMissing || !timing
                      ? UNAVAILABLE
                      : hours(timing.p90_seconds)
                  }
                />
                <Metric
                  label="Working time / issue p50"
                  value={
                    completionMissing || !completion?.working.count
                      ? UNAVAILABLE
                      : hours(completion.working.p50_seconds)
                  }
                />
                <Metric
                  label="Landed first try"
                  value={percent(
                    unavailable("landed_first_try")
                      ? null
                      : report.first_try_percent,
                  )}
                />
                <Metric
                  label="Cost / issue done"
                  value={
                    usageMissing || residenceMissing || !report.completion.done
                      ? UNAVAILABLE
                      : formatUsd(report.cost_usd / report.completion.done)
                  }
                  detail={
                    usageMissing
                      ? UNAVAILABLE
                      : `${formatTokens(report.tokens)} tokens · ${percent(report.input > 0 ? (100 * report.cached) / report.input : null)} cached`
                  }
                />
              </section>
              <Section
                title={
                  view === "system"
                    ? "Where system time goes"
                    : "Where lead time goes"
                }
                partial={p.lane_residence.partial}
              >
                {residenceMissing ? (
                  <Missing />
                ) : (
                  <>
                    <LaneBands lanes={lanes} view={view} />
                    {view === "lead" && (
                      <Missing>
                        Created-to-Done headline includes intake. Bands show
                        full lane residence for completed issues after first
                        Todo; Backlog and Triage are excluded.
                      </Missing>
                    )}
                    <LaneQuantiles lanes={lanes} />
                  </>
                )}
              </Section>
              <QualityMetrics quality={p.quality} />
              <Section
                title="Throughput"
                partial={unavailable("failed_attempts_complete_population")}
              >
                <ThroughputChart
                  buckets={report.throughput}
                  failedUnavailable={unavailable("failed_attempts")}
                  doneUnavailable={residenceMissing}
                />
                <Missing>
                  Issues done and failed attempts per hour · slots in use
                  (average).{" "}
                  {report.throughput[0] &&
                    `Buckets: ${(Date.parse(report.throughput[0].to) - Date.parse(report.throughput[0].from)) / 3600000}h.`}
                </Missing>
              </Section>
              <Section
                title="Aging work in progress"
                partial={
                  p.lane_residence.partial ||
                  p.lane_residence.aging.next_offset !== undefined
                }
              >
                {residenceMissing ? (
                  <Missing />
                ) : p.lane_residence.aging.items.length === 0 ? (
                  <Missing>No open work in the recorded population.</Missing>
                ) : (
                  <>
                    <AgingChart items={p.lane_residence.aging.items} />
                    <div className="flex flex-col gap-2 text-xs">
                      {p.lane_residence.aging.items
                        .toSorted((a, b) => b.hours - a.hours)
                        .slice(0, 5)
                        .map((item) => (
                          <div
                            key={item.work_item_id}
                            className="flex flex-wrap justify-between gap-2"
                          >
                            <IssueLink
                              id={item.work_item_id}
                              titles={report.titles}
                            />
                            <span className="tabular-nums text-muted-foreground">
                              {item.lane} · {item.hours.toFixed(1)} h
                            </span>
                          </div>
                        ))}
                    </div>
                    <Missing>
                      Amber: past lane p90 · red: past 24h · age at window end.
                    </Missing>
                  </>
                )}
              </Section>
              <Section
                title="Stages and models"
                partial={
                  unavailable("stage_and_issue_spend_complete_population") ||
                  unavailable("reports_usage_complete_population")
                }
              >
                <StageTable rows={report.stages} usageMissing={usageMissing} />
                <Missing>
                  Timing uses completed recorded phases; cache share uses input
                  tokens. Cost / issue uses distinct issues per row.{" "}
                  {unavailable("stage_gate_configuration") &&
                    "Project gate configuration is not recorded."}
                </Missing>
              </Section>
              <Section
                title="Rework by cause"
                partial={p.lane_residence.partial}
              >
                {residenceMissing ? (
                  <Missing />
                ) : p.lane_residence.rework_causes.length === 0 ? (
                  <Missing>
                    No recorded rework transitions in this window.
                  </Missing>
                ) : (
                  <div className="flex flex-col gap-2 text-xs">
                    {reworkByCause(p.lane_residence.rework_causes).map(
                      (cause) => (
                        <div
                          key={cause.label}
                          className="flex justify-between gap-3"
                        >
                          <span className="min-w-0 break-words">
                            <span title={cause.details}>{cause.label}</span>
                          </span>
                          <span className="tabular-nums">{cause.count}</span>
                        </div>
                      ),
                    )}
                  </div>
                )}
              </Section>
              <Section
                title="Cost concentration"
                partial={
                  unavailable("stage_and_issue_spend_complete_population") ||
                  unavailable("reports_usage_complete_population")
                }
              >
                {usageMissing ? (
                  <Missing />
                ) : report.spend.length === 0 ? (
                  <Missing>No issue spend recorded.</Missing>
                ) : (
                  <div className="flex flex-col gap-3">
                    {report.spend.slice(0, 10).map((item) => (
                      <div
                        key={item.work_item_id}
                        className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] gap-2 text-xs"
                      >
                        <IssueLink
                          id={item.work_item_id}
                          titles={report.titles}
                        />
                        <span className="tabular-nums">
                          {formatUsd(item.cost_usd)} ·{" "}
                          {percent(
                            report.cost_usd > 0
                              ? (100 * item.cost_usd) / report.cost_usd
                              : null,
                          )}
                        </span>
                        <svg
                          viewBox="0 0 100 3"
                          className="col-span-2 h-1 w-full"
                          preserveAspectRatio="none"
                          aria-hidden
                        >
                          <rect width="100" height="3" fill="var(--muted)" />
                          <rect
                            width={
                              report.cost_usd > 0
                                ? (100 * item.cost_usd) / report.cost_usd
                                : 0
                            }
                            height="3"
                            fill="var(--foreground)"
                          />
                        </svg>
                      </div>
                    ))}
                  </div>
                )}
              </Section>
              <Section
                title="Failure signatures"
                partial={
                  unavailable("failure_signatures_complete_population") ||
                  p.failure_signatures.next_offset !== undefined
                }
              >
                {p.failure_signatures.items.length === 0 ? (
                  <Missing>
                    No recorded failure signatures in this window.
                  </Missing>
                ) : (
                  <Table>
                    <TableHeader>
                      <TableRow>
                        {[
                          "Signature",
                          "Count",
                          "Issues",
                          "First / last seen",
                          "Wasted cost",
                          "Class",
                        ].map((h) => (
                          <TableHead key={h}>{h}</TableHead>
                        ))}
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {p.failure_signatures.items.map((f) => (
                        <TableRow key={f.signature}>
                          <TableCell>
                            <span className="block max-w-80 break-words">
                              {f.example}
                            </span>
                          </TableCell>
                          <TableCell className="tabular-nums">
                            {f.count}
                          </TableCell>
                          <TableCell>
                            <div className="flex min-w-40 flex-col gap-1">
                              {f.work_items.map((id) => (
                                <IssueLink
                                  key={id}
                                  id={id}
                                  titles={report.titles}
                                />
                              ))}
                              <span className="text-muted-foreground">
                                {f.work_items_count} issues
                                {f.work_items_partial ? " · Partial" : ""}
                              </span>
                            </div>
                          </TableCell>
                          <TableCell className="whitespace-nowrap">
                            {at(f.first_seen)}
                            <br />
                            {at(f.last_seen)}
                          </TableCell>
                          <TableCell className="tabular-nums">
                            {usageMissing ? UNAVAILABLE : formatUsd(f.cost_usd)}
                          </TableCell>
                          <TableCell>{f.class}</TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                )}
              </Section>
              <Section title="Source coverage">
                <div className="grid gap-2 text-xs sm:grid-cols-2">
                  {report.coverage.map((source) => (
                    <div
                      key={source.source}
                      className="flex flex-wrap items-center justify-between gap-2"
                    >
                      <span>{source.source}</span>
                      <span className="flex items-center gap-2 tabular-nums">
                        {source.observed ?? "—"} / {source.total ?? "—"}
                        <Badge
                          size="sm"
                          variant={
                            source.observed === null ||
                            source.total === null ||
                            source.observed < source.total ||
                            p.partial
                              ? "warning"
                              : "secondary"
                          }
                        >
                          {source.observed === null
                            ? "Unavailable"
                            : source.total === null ||
                                source.observed < source.total ||
                                p.partial
                              ? "Partial"
                              : "Observed"}
                        </Badge>
                      </span>
                    </div>
                  ))}
                </div>
                {report.unavailable.length > 0 && (
                  <div className="flex flex-wrap gap-2">
                    {report.unavailable.map((key) => (
                      <Badge key={key} size="sm" variant="outline">
                        {key.replaceAll("_", " ")} ·{" "}
                        {key.includes("complete_population") ||
                        key.includes("partial")
                          ? "Partial"
                          : "Unavailable"}
                      </Badge>
                    ))}
                  </div>
                )}
              </Section>
            </>
          )}
        </WorkspacePageContainer>
      </ScrollArea>
    </div>
  );
}

type Lane = ReportsReport["analytics"]["lane_residence"]["lanes"][number];
function LaneBands({
  lanes,
  view,
}: {
  lanes: readonly Lane[];
  view: ReportsTimeView;
}): React.ReactElement {
  const system = lanes.filter((l) => l.group === "system");
  const held = lanes.filter((l) => l.group === "held");
  const total = lanes.reduce((s, l) => s + l.seconds, 0);
  return (
    <div className="flex flex-col gap-3">
      {[
        { label: "Detent controls", lanes: system },
        { label: "Held by people", lanes: held },
      ].map((group) => {
        const sum = group.lanes.reduce((s, l) => s + l.seconds, 0);
        let x = 0;
        const denominator = view === "lead" ? total : sum;
        return (
          <div key={group.label} className="flex flex-col gap-2">
            <div className="flex justify-between text-xs">
              <span>{group.label}</span>
              <span className="tabular-nums">{hours(sum)}</span>
            </div>
            <svg
              viewBox="0 0 100 8"
              className="h-6 w-full"
              preserveAspectRatio="none"
              role="img"
              aria-label={`${group.label}: ${hours(sum)}`}
            >
              <rect width="100" height="8" fill="var(--muted)" />
              {group.lanes.map((lane) => {
                const width =
                  denominator > 0 ? (100 * lane.seconds) / denominator : 0;
                const start = x;
                x += width;
                return (
                  <rect
                    key={lane.lane}
                    x={start}
                    width={width}
                    height="8"
                    fill={laneColor(lane.lane)}
                  >
                    <title>
                      {lane.lane}: {hours(lane.seconds)}
                    </title>
                  </rect>
                );
              })}
            </svg>
            <div className="flex flex-wrap gap-x-4 gap-y-2 text-xs">
              {group.lanes.map((lane) => (
                <span key={lane.lane} className="flex items-center gap-1.5">
                  <span
                    className="size-2 shrink-0"
                    style={{ background: laneColor(lane.lane) }}
                  />
                  {lane.lane}{" "}
                  <span className="tabular-nums text-muted-foreground">
                    {hours(lane.seconds)}
                  </span>
                </span>
              ))}
            </div>
          </div>
        );
      })}
    </div>
  );
}
function LaneQuantiles({
  lanes,
}: {
  lanes: readonly Lane[];
}): React.ReactElement {
  const max = Math.max(1, ...lanes.map((l) => l.p90_seconds));
  return (
    <div className="grid gap-2 pt-2">
      {lanes.map((lane) => (
        <div
          key={lane.lane}
          className="grid grid-cols-[6rem_minmax(0,1fr)_7rem] items-center gap-2 text-xs"
        >
          <span>{lane.lane}</span>
          <svg
            viewBox="0 0 100 6"
            className="h-4 w-full"
            preserveAspectRatio="none"
            role="img"
            aria-label={`${lane.lane} p50 ${hours(lane.p50_seconds)} to p90 ${hours(lane.p90_seconds)}`}
          >
            <rect
              width={(100 * lane.p90_seconds) / max}
              height="6"
              fill={laneColor(lane.lane)}
              opacity="0.3"
            />
            <rect
              width={(100 * lane.p50_seconds) / max}
              height="6"
              fill={laneColor(lane.lane)}
            />
          </svg>
          <span className="whitespace-nowrap text-right tabular-nums">
            {compactDuration(lane.p50_seconds)} →{" "}
            {compactDuration(lane.p90_seconds)}
          </span>
        </div>
      ))}
    </div>
  );
}
function ThroughputChart({
  buckets,
  failedUnavailable,
  doneUnavailable,
}: {
  buckets: ReportsReport["throughput"];
  failedUnavailable: boolean;
  doneUnavailable: boolean;
}): React.ReactElement {
  if (buckets.length === 0) return <Missing />;
  const values = buckets.map((b) => ({
    ...b,
    hours: (Date.parse(b.to) - Date.parse(b.from)) / 3600000,
  }));
  const max = Math.max(
    1,
    ...values.flatMap((b) => [
      b.done / b.hours,
      b.failed / b.hours,
      b.slots ?? 0,
    ]),
  );
  const step = 600 / values.length;
  const y = (n: number) => 148 - (n / max) * 120;
  const points = values
    .map((b, i) =>
      b.slots === null ? null : `${30 + (i + 0.5) * step},${y(b.slots)}`,
    )
    .filter((p) => p !== null)
    .join(" ");
  return (
    <>
      <svg
        viewBox="0 0 660 180"
        className="w-full"
        role="img"
        aria-label="Throughput per hour and average slots in use"
      >
        <text x="2" y="25" fill="var(--muted-foreground)" fontSize="11">
          {max.toFixed(1)}
        </text>
        <text x="12" y="148" fill="var(--muted-foreground)" fontSize="11">
          0
        </text>
        <line x1="30" y1="148" x2="630" y2="148" stroke="var(--border)" />
        {values.map((b, i) => (
          <g key={b.from}>
            {!doneUnavailable && (
              <>
                <rect
                  x={30 + i * step}
                  y={y(b.done / b.hours)}
                  width={Math.max(0.5, step * 0.42)}
                  height={148 - y(b.done / b.hours)}
                  fill="var(--success)"
                >
                  <title>
                    {at(b.from)} · {b.done / b.hours} done/h
                  </title>
                </rect>
              </>
            )}
            {!failedUnavailable && (
              <rect
                x={30 + i * step + step * 0.44}
                y={y(b.failed / b.hours)}
                width={Math.max(0.5, step * 0.42)}
                height={148 - y(b.failed / b.hours)}
                fill="var(--destructive)"
              >
                <title>
                  {at(b.from)} · {b.failed / b.hours} failed/h
                </title>
              </rect>
            )}
          </g>
        ))}
        <polyline
          points={points}
          fill="none"
          stroke="var(--foreground)"
          strokeWidth="1.5"
        />
        <text x="30" y="173" fill="var(--muted-foreground)" fontSize="11">
          {at(buckets[0]!.from)}
        </text>
        <text
          x="630"
          y="173"
          textAnchor="end"
          fill="var(--muted-foreground)"
          fontSize="11"
        >
          {at(buckets[buckets.length - 1]!.to)}
        </text>
      </svg>
      <div className="flex flex-wrap gap-4 text-xs">
        <span className="text-success-foreground">
          Issues done / h{doneUnavailable ? " · Not recorded" : ""}
        </span>
        <span className="text-destructive-foreground">
          Failed attempts / h{failedUnavailable ? " · Not recorded" : ""}
        </span>
        <span>
          Slots in use
          {values.every((b) => b.slots === null) ? " · Not recorded" : ""}
        </span>
      </div>
    </>
  );
}
function AgingChart({
  items,
}: {
  items: ReportsReport["analytics"]["lane_residence"]["aging"]["items"];
}): React.ReactElement {
  const lanes = [...new Set(items.map((i) => i.lane))];
  const max = Math.max(24, ...items.map((i) => i.hours));
  return (
    <svg
      viewBox={`0 0 660 ${lanes.length * 30 + 28}`}
      className="w-full"
      role="img"
      aria-label="Open issue ages in current lane"
    >
      {lanes.map((lane, i) => (
        <g key={lane}>
          <text
            x="0"
            y={i * 30 + 19}
            fontSize="11"
            fill="var(--muted-foreground)"
          >
            {lane}
          </text>
          <line
            x1="100"
            y1={i * 30 + 15}
            x2="640"
            y2={i * 30 + 15}
            stroke="var(--border)"
          />
        </g>
      ))}
      {items.map((item) => (
        <circle
          key={item.work_item_id}
          cx={100 + (540 * item.hours) / max}
          cy={lanes.indexOf(item.lane) * 30 + 15}
          r="4"
          fill={
            item.hours > 24
              ? "var(--destructive)"
              : item.lane_p90_hours !== undefined &&
                  item.hours > item.lane_p90_hours
                ? "var(--warning)"
                : laneColor(item.lane)
          }
        >
          <title>
            {item.work_item_id} · {item.lane} · {item.hours.toFixed(1)} h
          </title>
        </circle>
      ))}
      <text
        x="100"
        y={lanes.length * 30 + 18}
        fontSize="11"
        fill="var(--muted-foreground)"
      >
        0h
      </text>
      <text
        x="640"
        y={lanes.length * 30 + 18}
        textAnchor="end"
        fontSize="11"
        fill="var(--muted-foreground)"
      >
        {max.toFixed(1)}h
      </text>
    </svg>
  );
}
function StageTable({
  rows,
  usageMissing,
}: {
  rows: ReportsReport["stages"];
  usageMissing: boolean;
}): React.ReactElement {
  if (rows.length === 0)
    return <Missing>No recorded sessions in this window.</Missing>;
  return (
    <Table>
      <TableHeader>
        <TableRow>
          {[
            "Stage / model / effort",
            "Sessions",
            "p50",
            "p90",
            "Succeeded",
            "Tokens",
            "Cached",
            "Cost",
            "Cost / issue",
          ].map((h) => (
            <TableHead key={h}>{h}</TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={`${row.stage}:${row.model}:${row.effort}`}>
            <TableCell>
              <div className="min-w-40">
                <span className="inline-flex items-center gap-2">
                  {row.stage}
                  {!row.disabled && row.usage_observed < row.sessions && (
                    <Badge variant="warning" size="sm">
                      Partial
                    </Badge>
                  )}
                </span>
                {!row.disabled && (
                  <>
                    <br />
                    <span className="text-muted-foreground">
                      {row.model} · {row.effort}
                    </span>
                  </>
                )}
              </div>
            </TableCell>
            {row.disabled ? (
              <TableCell colSpan={8}>Disabled by project gates</TableCell>
            ) : (
              <>
                {[
                  formatCount(row.sessions),
                  row.duration.count
                    ? hours(row.duration.p50_seconds)
                    : UNAVAILABLE,
                  row.duration.count
                    ? hours(row.duration.p90_seconds)
                    : UNAVAILABLE,
                  percent(
                    row.sessions ? (100 * row.succeeded) / row.sessions : null,
                  ),
                  usageMissing || !row.usage_observed
                    ? UNAVAILABLE
                    : formatTokens(row.tokens),
                  percent(
                    !usageMissing && row.input > 0
                      ? (100 * row.cached) / row.input
                      : null,
                  ),
                  usageMissing || !row.usage_observed
                    ? UNAVAILABLE
                    : formatUsd(row.cost_usd),
                  usageMissing || !row.usage_observed || !row.issues
                    ? UNAVAILABLE
                    : formatUsd(row.cost_usd / row.issues),
                ].map((value, i) => (
                  <TableCell key={i} className="whitespace-nowrap tabular-nums">
                    {value}
                  </TableCell>
                ))}
              </>
            )}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

const QUALITY_CAUSES = [
  ["underspecified_issue", "Underspecified issue"],
  ["missing_criterion", "Missing criterion"],
  ["validator_miss", "Validator miss"],
  ["infrastructure", "Infrastructure"],
] as const;

export function QualityMetrics({ quality }: {
  quality: ReportsReport["analytics"]["quality"];
}): React.ReactElement {
  return (
    <Section title="Rework and escapes" partial={quality?.partial}>
      {quality === undefined ? <Missing /> : <>
        <div className="grid grid-cols-2 gap-4">
          <Metric label="Rework rate" value={percent(quality.rework_percent)}
            detail={`${formatCount(quality.reworked_items)} of ${formatCount(quality.worked_items)} worked items returned before landing`} />
          <Metric label="Escape rate" value={percent(quality.escape_percent)}
            detail={`${formatCount(quality.escaped_versions)} of ${formatCount(quality.landed_versions)} versions landed in this window`} />
        </div>
        <Missing>
          Escape rate follows the landing cohort through the end of this window.
          Cause counts follow detection time, including earlier landings.
          Infrastructure is attributed to the instance and excluded from the escape rate.
        </Missing>
        {quality.pending_classification > 0 && <Missing>
          {formatCount(quality.pending_classification)} escapes await cause classification and are included in the escape rate.
        </Missing>}
        <Table>
          <TableHeader><TableRow>
            <TableHead>Window start</TableHead>
            <TableHead className="text-right">Rework rate</TableHead>
            <TableHead className="text-right">Escape rate</TableHead>
            {QUALITY_CAUSES.map(([key, label]) => <TableHead key={key} className="text-right">{label}</TableHead>)}
            <TableHead className="text-right">Awaiting classification</TableHead>
          </TableRow></TableHeader>
          <TableBody>
            {quality.buckets.filter((bucket) => bucket.worked_items > 0 || bucket.landed_versions > 0 || bucket.escapes > 0).map((bucket) => <TableRow key={bucket.from}>
              <TableCell>{at(bucket.from)}</TableCell>
              <TableCell className="text-right tabular-nums">{percent(bucket.rework_percent)}</TableCell>
              <TableCell className="text-right tabular-nums">{percent(bucket.escape_percent)}</TableCell>
              {QUALITY_CAUSES.map(([key]) => <TableCell key={key} className="text-right tabular-nums">{formatCount(bucket.causes[key] ?? 0)}</TableCell>)}
              <TableCell className="text-right tabular-nums">{formatCount(bucket.pending_classification)}</TableCell>
            </TableRow>)}
            <TableRow>
              <TableCell>Total in window</TableCell>
              <TableCell className="text-right tabular-nums">{percent(quality.rework_percent)}</TableCell>
              <TableCell className="text-right tabular-nums">{percent(quality.escape_percent)}</TableCell>
              {QUALITY_CAUSES.map(([key]) => <TableCell key={key} className="text-right tabular-nums">{formatCount(quality.causes[key] ?? 0)}</TableCell>)}
              <TableCell className="text-right tabular-nums">{formatCount(quality.pending_classification)}</TableCell>
            </TableRow>
          </TableBody>
        </Table>
        {quality.escapes === 0 && <Missing>No recorded escapes in this window.</Missing>}
      </>}
    </Section>
  );
}
