import { useEffect, useState } from "react";
import { Link } from "@tanstack/react-router";
import { GitPullRequestIcon, TerminalIcon } from "lucide-react";
import { RunnerStatusDot } from "../../components/RunnerStatusDot.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import { Button, InlineButton } from "../../components/ui/button.tsx";
import { ScrollArea } from "../../components/ui/scroll-area.tsx";
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectPopup,
  SelectItem,
} from "../../components/ui/select.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Toggle } from "../../components/ui/toggle.tsx";
import {
  Tooltip,
  TooltipPopup,
  TooltipTrigger,
} from "../../components/ui/tooltip.tsx";
import type { AccountProject, FleetRunner } from "../../contracts/account.ts";
import type { ActivityReport } from "../../contracts/activity.ts";
import {
  formatChatTimestampTooltip,
  formatRelativeTimeLabel,
  formatShortTimestamp,
} from "../../timestampFormat.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { StatusDot } from "../account/controls.tsx";
import { useClientSettings } from "../adapters/settings.ts";
import { fleetHosts } from "../fleet/capacity.ts";
import { usePageTitle } from "../pageTitle.ts";
import { allProjectsLabel } from "../lib/projectScope.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import {
  activityGroups,
  runnerCapacity,
  runnerCategory,
  runnerName,
  displayDuration,
  issueTiming,
  laneTiming,
  issueMeta,
  type ActivityIssue,
  type Group,
  type Sort,
} from "./activityModel.ts";

import {
  activityKey,
  observeActivity,
  type ActivityState,
} from "./activityResource.ts";

export function ActivityRoute() {
  usePageTitle("Activity");
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const http = useWorkHttp();
  const [project, setProject] = useState("");
  const [live, setLive] = useState(false);
  const [state, setState] = useState<ActivityState & { readonly api?: object }>(
    { report: null, runners: [], pending: true, error: null },
  );
  useEffect(() => {
    const projects = (bootstrap?.projects ?? [])
      .filter((row) => !project || row.id === project)
      .map((row) => row.id);
    setLive(false);
    const resource = observeActivity(
      api,
      activityKey(project, projects),
      project,
      (next) =>
        setState((previous) =>
          next.report === null &&
          next.pending &&
          previous.api === api &&
          previous.report !== null
            ? { ...previous, pending: true, error: null }
            : { ...next, api },
        ),
      http,
      projects,
      setLive,
    );
    return () => resource.close();
  }, [api, bootstrap?.projects, http, project]);
  return (
    <ActivityView
      report={state.report}
      runners={state.runners}
      pending={state.pending && state.report === null}
      error={state.error}
      projects={bootstrap?.projects ?? []}
      organizationName={bootstrap?.organization?.name}
      project={project}
      onProjectChange={setProject}
      live={live}
    />
  );
}

function Picker<T extends string>({
  label,
  value,
  options,
  onChange,
}: {
  readonly label: string;
  readonly value: T;
  readonly options: readonly { value: T; label: string }[];
  readonly onChange: (value: T) => void;
}) {
  return (
    <Select
      value={value}
      items={options}
      onValueChange={(next) => {
        if (next !== null) onChange(next);
      }}
    >
      <SelectTrigger
        size="compact"
        aria-label={label}
        className="w-auto min-w-0 max-w-full"
      >
        <SelectValue />
      </SelectTrigger>
      <SelectPopup>
        {options.map((option) => (
          <SelectItem key={option.value} value={option.value}>
            {option.label}
          </SelectItem>
        ))}
      </SelectPopup>
    </Select>
  );
}

export function ActivityView({
  report,
  runners,
  projects,
  organizationName,
  pending,
  error,
  live,
  project,
  onProjectChange,
}: {
  readonly report: ActivityReport | null;
  readonly runners: readonly FleetRunner[];
  readonly projects: readonly AccountProject[];
  readonly organizationName?: string;
  readonly pending: boolean;
  readonly error: string | null;
  readonly live: boolean;
  readonly project: string;
  readonly onProjectChange: (value: string) => void;
}) {
  const [runner, setRunner] = useState("");
  const allProjects = allProjectsLabel(organizationName);
  const [group, setGroup] = useState<Group>("none");
  const [sort, setSort] = useState<Sort>("attention");
  const [clock, setClock] = useState(false);
  const [now, setNow] = useState(Date.now);
  const timestampFormat = useClientSettings(
    (settings) => settings.timestampFormat,
  );
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 15_000);
    return () => clearInterval(timer);
  }, []);
  const scope = projects.find((row) => row.id === project)?.name ?? allProjects;
  const selectedRunner = runners.find((row) => row.id === runner);
  const working = runners.filter(
    (row) => runnerCategory(row) === "working",
  ).length;
  const idle = runners.filter((row) => runnerCategory(row) === "idle").length;
  const hosts = fleetHosts(runners);
  const used = hosts.reduce((sum, host) => sum + host.used, 0);
  const slots = hosts.reduce((sum, host) => sum + host.capacity, 0);
  const slowRunners = new Set(
    (report?.running_issues ?? [])
      .filter(
        (row) =>
          report &&
          (issueTiming(row, report, now, false).slow ||
            row.lanes.some(
              (lane) => laneTiming(lane, row, report, now, false).slow,
            )),
      )
      .flatMap((row) => row.runner_ids),
  );
  const orderedRunners = runners.toSorted(
    (a, b) =>
      ["working", "idle", "offline"].indexOf(runnerCategory(a)) -
        ["working", "idle", "offline"].indexOf(runnerCategory(b)) ||
      runnerName(a).localeCompare(runnerName(b)),
  );
  const timeButton = (at: string, prefix: string) => (
    <Tooltip>
      <TooltipTrigger
        render={
          <InlineButton
            tone="muted"
            onClick={() => setClock((value) => !value)}
            aria-label={`${prefix} ${clock ? `at ${formatShortTimestamp(at, timestampFormat)}` : formatRelativeTimeLabel(at)}; show all times as ${clock ? "relative times" : "clock times"}`}
          />
        }
      >
        <span className={prefix === "Done" ? "text-success-foreground" : ""}>
          {prefix}
        </span>{" "}
        <time dateTime={at}>
          {clock
            ? `at ${formatShortTimestamp(at, timestampFormat)}`
            : formatRelativeTimeLabel(at)}
        </time>
      </TooltipTrigger>
      <TooltipPopup>
        {formatChatTimestampTooltip(at, timestampFormat)}
      </TooltipPopup>
    </Tooltip>
  );
  const renderRow = (issue: ActivityIssue, finished: boolean) => {
    if (!report) return null;
    const metrics = issueTiming(issue, report, now, finished);
    const meta = issueMeta(issue, finished);
    const totalDetail = (
      <>
        <div>Total {displayDuration(metrics.total)}</div>
        <div>
          Since it entered {issue.clock_lane}{" "}
          {issue.clock_started_at
            ? formatChatTimestampTooltip(
                issue.clock_started_at,
                timestampFormat,
              )
            : "at an unavailable time"}
        </div>
        <div>{metrics.baselineText}</div>
      </>
    );
    const workedDetail = (
      <>
        <div>
          Worked {displayDuration(metrics.worked)} · waiting{" "}
          {displayDuration(metrics.waiting)}
        </div>
        <table className="mt-2 w-full text-xs">
          <thead>
            <tr>
              <th className="text-left font-normal">Role</th>
              <th className="px-3 font-normal">Attempts</th>
              <th className="text-right font-normal">Time</th>
            </tr>
          </thead>
          <tbody>
            {issue.worked_by_role.map((role) => (
              <tr key={role.role}>
                <td>
                  {role.role}
                  {role.running ? " · running" : ""}
                </td>
                <td className="px-3 text-center">{role.attempts}</td>
                <td className="text-right">{displayDuration(role.seconds)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </>
    );
    const host = runners.find((row) => row.id === issue.runner_id);
    const prNumber = issue.pull_request_url?.match(
      /\/pull\/(\d+)(?:[/?#]|$)/,
    )?.[1];
    return (
      <div
        role="row"
        key={issue.work_item_id}
        className="grid min-w-0 grid-cols-1 gap-2 border-b border-border py-3 @[1100px]/activity:grid-cols-[minmax(12rem,1fr)_8rem_minmax(16rem,1.2fr)_4.5rem_6.5rem_10rem] @[1100px]/activity:items-center @[1100px]/activity:gap-x-6 @[1100px]/activity:py-2"
      >
        <div role="cell" className="min-w-0">
          <Tooltip>
            <TooltipTrigger
              render={
                <Link
                  to="/work/i/$workItemId"
                  params={{ workItemId: issue.work_item_id }}
                  className="block truncate text-sm hover:underline"
                />
              }
            >
              <span className="text-muted-foreground">#{issue.number}</span>{" "}
              {issue.title}
            </TooltipTrigger>
            <TooltipPopup>{issue.title}</TooltipPopup>
          </Tooltip>
          <div className="mt-0.5 flex min-w-0 items-center gap-x-1 text-xs text-muted-foreground">
            <span className="min-w-0 truncate">{issue.project_name} ·</span>
            <span
              className={
                finished && issue.terminal_lane === "Done"
                  ? "text-success-foreground"
                  : ""
              }
            >
              {meta.at
                ? timeButton(meta.at, meta.prefix)
                : "Timing unavailable"}
            </span>
          </div>
        </div>
        <div role="cell" className="flex min-w-0 items-center gap-2 text-xs">
          <RunnerStatusDot runner={host} />
          <Tooltip>
            <TooltipTrigger
              render={<span className="min-w-0 truncate" tabIndex={0} />}
            >
              {host
                ? runnerName(host)
                : issue.runner_id || "Runner unavailable"}
              {issue.runner_ids.length > 1
                ? ` +${issue.runner_ids.length - 1}`
                : ""}
            </TooltipTrigger>
            <TooltipPopup>
              {issue.runner_ids
                .map((id) => {
                  const runner = runners.find((row) => row.id === id);
                  return runner ? runnerName(runner) : id;
                })
                .join(", ")}
            </TooltipPopup>
          </Tooltip>
          <span className="ms-auto flex shrink-0 items-center gap-1 tabular-nums @[1100px]/activity:hidden">
            <Tooltip>
              <TooltipTrigger
                render={
                  <span
                    tabIndex={0}
                    className={
                      metrics.slow
                        ? "font-medium text-warning-foreground"
                        : "font-medium"
                    }
                  />
                }
              >
                {issue.clock_started_at ? displayDuration(metrics.total) : "—"}
              </TooltipTrigger>
              <TooltipPopup>{totalDetail}</TooltipPopup>
            </Tooltip>{" "}
            total ·
            <Tooltip>
              <TooltipTrigger render={<span tabIndex={0} />}>
                {displayDuration(metrics.worked)} worked
              </TooltipTrigger>
              <TooltipPopup>{workedDetail}</TooltipPopup>
            </Tooltip>
          </span>
        </div>
        <div role="cell" className="flex min-w-0 gap-1">
          {issue.lanes.map((lane) => {
            const metric = laneTiming(lane, issue, report, now, finished);
            return (
              <Tooltip key={lane.name}>
                <TooltipTrigger
                  render={
                    <div
                      tabIndex={0}
                      className="min-w-0 flex-1 rounded-xs text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring"
                      aria-label={`${lane.name}, ${metric.visited ? displayDuration(metric.seconds) : "not visited"}. ${metric.baselineText}`}
                    />
                  }
                >
                  <div
                    className={`flex min-w-0 gap-1 ${metric.visited ? "text-muted-foreground" : "text-muted-foreground/60"}`}
                  >
                    <span className="truncate">{lane.name}</span>
                    {lane.entries > 1 ? (
                      <span className="shrink-0">×{lane.entries}</span>
                    ) : null}
                  </div>
                  <div
                    className={`tabular-nums ${metric.slow ? "text-warning-foreground" : !metric.visited ? "text-muted-foreground/60" : ""}`}
                  >
                    {metric.visited ? displayDuration(metric.seconds) : "—"}
                  </div>
                  <div
                    className="mt-0.5 h-1 overflow-hidden rounded-full bg-muted"
                    aria-hidden
                  >
                    <div
                      className={`h-full rounded-full ${metric.tone === "warning" ? "bg-warning" : metric.tone === "working" ? "bg-info" : "bg-muted-foreground/60"}`}
                      style={{
                        width: metric.visited ? "100%" : "0%",
                        ...(lane.role === "holding"
                          ? {
                              backgroundImage:
                                "repeating-linear-gradient(135deg, transparent, transparent 2px, var(--background) 2px, var(--background) 4px)",
                            }
                          : {}),
                      }}
                    />
                  </div>
                </TooltipTrigger>
                <TooltipPopup>
                  <div>
                    {lane.name} · {displayDuration(metric.seconds)}
                  </div>
                  <div>{metric.baselineText}</div>
                </TooltipPopup>
              </Tooltip>
            );
          })}
        </div>
        <div role="cell" className="hidden text-right @[1100px]/activity:block">
          <Tooltip>
            <TooltipTrigger
              render={
                <span
                  tabIndex={0}
                  aria-label={`Total ${displayDuration(metrics.total)}`}
                  className={`text-sm font-medium tabular-nums ${metrics.slow ? "text-warning-foreground" : ""}`}
                />
              }
            >
              {issue.clock_started_at ? displayDuration(metrics.total) : "—"}
            </TooltipTrigger>
            <TooltipPopup>{totalDetail}</TooltipPopup>
          </Tooltip>
        </div>
        <div role="cell" className="hidden text-right @[1100px]/activity:block">
          <Tooltip>
            <TooltipTrigger
              render={
                <div
                  tabIndex={0}
                  aria-label={`Worked ${displayDuration(metrics.worked)}, ${Math.round(metrics.share * 100)}%`}
                  className="text-xs tabular-nums"
                />
              }
            >
              <span className="font-medium">
                {displayDuration(metrics.worked)}
              </span>{" "}
              <span className="text-muted-foreground">
                {Math.round(metrics.share * 100)}%
              </span>
              <div
                className="mt-1 h-1 overflow-hidden rounded-full bg-muted"
                aria-hidden
              >
                <div
                  className={`h-full rounded-full ${metrics.workedTone === "warning" ? "bg-warning" : "bg-info"}`}
                  style={{ width: `${metrics.share * 100}%` }}
                />
              </div>
            </TooltipTrigger>
            <TooltipPopup>{workedDetail}</TooltipPopup>
          </Tooltip>
        </div>
        <div
          role="cell"
          className="hidden items-center gap-3 text-xs @[1100px]/activity:flex"
        >
          <span className="shrink-0">
            {issue.change_id ? (
              <Link
                to="/work/i/$workItemId/changes/$changeId"
                params={{
                  workItemId: issue.work_item_id,
                  changeId: issue.change_id,
                }}
                className="inline-flex items-center gap-1.5 hover:underline"
              >
                <GitPullRequestIcon
                  aria-hidden
                  className="size-3.5 text-muted-foreground"
                />
                {prNumber ? `PR #${prNumber}` : "Change"}
              </Link>
            ) : issue.pull_request_url ? (
              <a
                href={issue.pull_request_url}
                className="inline-flex items-center gap-1.5 hover:underline"
              >
                <GitPullRequestIcon
                  aria-hidden
                  className="size-3.5 text-muted-foreground"
                />
                {prNumber ? `PR #${prNumber}` : "PR"}
              </a>
            ) : (
              <span className="text-muted-foreground">
                {finished ? "No PR" : "No PR yet"}
              </span>
            )}
          </span>
          <span className="shrink-0">
            {issue.session_id ? (
              <Link
                to="/work/i/$workItemId"
                params={{ workItemId: issue.work_item_id }}
                search={{ panel: "conversation" }}
                className="inline-flex items-center gap-1.5 hover:underline"
              >
                <TerminalIcon
                  aria-hidden
                  className="size-3.5 text-muted-foreground"
                />
                Session
              </Link>
            ) : (
              <span className="text-muted-foreground">No session</span>
            )}
          </span>
        </div>
      </div>
    );
  };
  const section = (finished: boolean) => {
    if (!report) return null;
    const groups = activityGroups(
      finished ? (report.finished_issues ?? []) : (report.running_issues ?? []),
      report,
      runners,
      project,
      runner,
      finished ? "none" : group,
      sort,
      now,
      finished,
    );
    const rows = groups.flatMap((entry) => entry.rows);
    const slow = rows.filter(
      (row) =>
        issueTiming(row, report, now, finished).slow ||
        row.lanes.some(
          (lane) => laneTiming(lane, row, report, now, finished).slow,
        ),
    ).length;
    const title = finished ? "Finished" : "Working now";
    const outcomes = new Map<string, number>();
    for (const row of rows) {
      if (row.terminal_lane)
        outcomes.set(
          row.terminal_lane,
          (outcomes.get(row.terminal_lane) ?? 0) + 1,
        );
    }
    return (
      <section
        aria-label={title}
        className={finished ? "border-t border-border pt-5" : ""}
      >
        <div className="mb-2 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-xs">
          <h2 className="text-sm font-medium text-muted-foreground">{title}</h2>
          <span>
            {rows.length} {rows.length === 1 ? "issue" : "issues"}
            {finished
              ? ` in ${Math.round((Date.parse(report.window.to) - Date.parse(report.window.from)) / 86_400_000)} days`
              : ""}
          </span>
          {finished
            ? [...outcomes].map(([lane, count]) => (
                <span
                  key={lane}
                  className={
                    lane === "Done"
                      ? "text-success-foreground"
                      : "text-muted-foreground"
                  }
                >
                  {count} {lane.toLowerCase()}
                </span>
              ))
            : null}
          {slow > 0 ? (
            <span className="text-warning-foreground">
              {slow} slower than usual
            </span>
          ) : null}
        </div>
        {rows.length === 0 ? (
          <p className="py-3 text-sm text-muted-foreground">
            {finished
              ? "No recent finishes for this filter."
              : "Nothing is running for this filter."}
          </p>
        ) : (
          <div
            role="table"
            aria-label={finished ? "Finished issues" : "Working issues"}
          >
            <div
              role="row"
              className="hidden grid-cols-[minmax(12rem,1fr)_8rem_minmax(16rem,1.2fr)_4.5rem_6.5rem_10rem] gap-x-6 border-b border-border pb-1 text-xs text-muted-foreground @[1100px]/activity:grid"
            >
              {[
                "Issue",
                "Runner",
                "Time in each lane",
                "Total",
                "Worked",
                "Links",
              ].map((label) => (
                <span
                  role="columnheader"
                  key={label}
                  className={
                    label === "Total" || label === "Worked" ? "text-right" : ""
                  }
                >
                  {label}
                </span>
              ))}
            </div>
            {groups.map((entry) => (
              <div role="rowgroup" key={entry.id}>
                {!finished && group !== "none" ? (
                  <div
                    role="row"
                    className="border-b border-border bg-muted/40 px-2 py-1.5 text-xs"
                  >
                    <span role="cell">
                      {entry.label} · {entry.rows.length}{" "}
                      {entry.rows.length === 1 ? "issue" : "issues"}
                    </span>
                  </div>
                ) : null}
                {entry.rows.map((attempt) => renderRow(attempt, finished))}
              </div>
            ))}
          </div>
        )}
      </section>
    );
  };
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-background text-foreground">
      <WorkspacePageHeader>
        <WorkspaceBreadcrumb
          ariaLabel="Activity breadcrumb"
          className="min-w-0"
        >
          <WorkspaceBreadcrumbItem>
            <h1>Activity</h1>
          </WorkspaceBreadcrumbItem>
          <WorkspaceBreadcrumbSeparator />
          <WorkspaceBreadcrumbItem current={!runner} className="shrink">
            <Tooltip>
              <TooltipTrigger
                render={<span className="truncate" tabIndex={0} />}
              >
                {scope}
              </TooltipTrigger>
              <TooltipPopup side="bottom" className="max-w-80">
                {scope}
              </TooltipPopup>
            </Tooltip>
          </WorkspaceBreadcrumbItem>
          {runner ? (
            <>
              <WorkspaceBreadcrumbSeparator />
              <WorkspaceBreadcrumbItem current>
                <span className="truncate">
                  {selectedRunner ? runnerName(selectedRunner) : runner}
                </span>
              </WorkspaceBreadcrumbItem>
            </>
          ) : null}
        </WorkspaceBreadcrumb>
        <span
          className="ms-auto flex shrink-0 items-center gap-2 text-xs text-muted-foreground"
          role="status"
        >
          <StatusDot tone={live ? "ok" : "idle"} />
          {live ? "Live" : "Reconnecting"}
        </span>
      </WorkspacePageHeader>
      <ScrollArea className="min-h-0 flex-1">
        <WorkspacePageContainer width="expanded">
          <div className="@container/activity flex min-w-0 flex-col gap-6">
            {pending || error || runners.length > 0 ? (
              <div
                className="flex flex-wrap items-center gap-2"
                aria-label="Activity filters"
              >
                <Picker
                  label="Project"
                  value={project}
                  onChange={onProjectChange}
                  options={[
                    { value: "", label: allProjects },
                    ...projects.map((row) => ({
                      value: row.id,
                      label: row.name,
                    })),
                  ]}
                />
                <Picker
                  label="Runner"
                  value={runner}
                  onChange={setRunner}
                  options={[
                    { value: "", label: "All runners" },
                    ...orderedRunners.map((row) => ({
                      value: row.id,
                      label: runnerName(row),
                    })),
                  ]}
                />
                <div className="flex w-full flex-col items-start gap-2 @[600px]/activity:ms-auto @[600px]/activity:w-auto @[600px]/activity:flex-row @[600px]/activity:items-center">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="text-xs text-muted-foreground">Group</span>
                    <Picker
                      label="Group"
                      value={group}
                      onChange={setGroup}
                      options={[
                        { value: "none", label: "None" },
                        { value: "runner", label: "Runner" },
                        { value: "project", label: "Project" },
                        { value: "lane", label: "Lane" },
                      ]}
                    />
                  </div>
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="text-xs text-muted-foreground">Sort</span>
                    <Picker
                      label="Sort"
                      value={sort}
                      onChange={setSort}
                      options={[
                        { value: "attention", label: "Needs attention first" },
                        { value: "longest", label: "Longest total" },
                        { value: "started", label: "Most recently started" },
                        { value: "number", label: "Issue number" },
                      ]}
                    />
                  </div>
                </div>
              </div>
            ) : null}
            {pending ? (
              <div
                aria-label="Loading activity"
                className="flex flex-col gap-3"
              >
                <Skeleton className="h-12" />
                {Array.from({ length: 5 }, (_, index) => (
                  <Skeleton key={index} className="h-12" />
                ))}
              </div>
            ) : error ? (
              <p role="status" className="text-sm text-muted-foreground">
                {error}
              </p>
            ) : runners.length === 0 ? (
              <div className="flex flex-col items-start gap-3">
                <p className="text-sm text-muted-foreground">
                  No runners enrolled yet. Runners pick up work and show here
                  while they run.
                </p>
                <Button
                  size="compact"
                  variant="outline"
                  render={
                    <Link
                      to="/settings/$section"
                      params={{ section: "runners" }}
                    />
                  }
                >
                  Enroll a runner
                </Button>
              </div>
            ) : (
              <>
                {report?.partial || report?.issues_partial ? (
                  <p className="text-sm text-muted-foreground">
                    Activity is incomplete; some attempts or timing data may be
                    missing.
                  </p>
                ) : null}
                <section aria-label="Runners" className="space-y-2">
                  <h2 className="text-xs font-normal text-muted-foreground">
                    Runners · {working} working · {idle} idle ·{" "}
                    {runners.length - working - idle} offline · {used} of{" "}
                    {slots} slots in use
                  </h2>
                  <div className="flex flex-wrap gap-1.5">
                    {orderedRunners.map((host) => (
                      <Toggle
                        key={host.id}
                        size="compact"
                        variant="pill-outline"
                        pressed={runner === host.id}
                        onPressedChange={(pressed) =>
                          setRunner(pressed ? host.id : "")
                        }
                        aria-label={`Filter by ${runnerName(host)}`}
                      >
                        <RunnerStatusDot runner={host} />
                        {runnerName(host)}{" "}
                        <span
                          className={
                            slowRunners.has(host.id)
                              ? "text-warning-foreground"
                              : "text-muted-foreground"
                          }
                        >
                          {runnerCategory(host) === "offline"
                            ? "offline"
                            : `${host.leases.length}/${runnerCapacity(host)}`}
                        </span>
                      </Toggle>
                    ))}
                  </div>
                </section>
                {section(false)}
                {section(true)}
              </>
            )}
          </div>
        </WorkspacePageContainer>
      </ScrollArea>
    </div>
  );
}
