import { useEffect, useState } from "react";
import { Link } from "@tanstack/react-router";
import { GitPullRequestIcon, TerminalIcon } from "lucide-react";
import { RunnerStatusDot } from "../../components/RunnerStatusDot.tsx";
import { StageProgress } from "../../components/StageProgress.tsx";
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
import { subscribeProjectEvents } from "../work/lib/projectEvents.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import {
  activityGroups,
  runnerCapacity,
  runnerCategory,
  runnerName,
  STAGES,
  timing,
  type ActivityAttempt,
  type Group,
  type Sort,
} from "./activityModel.ts";

export function ActivityRoute() {
  usePageTitle("Activity");
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const http = useWorkHttp();
  const [project, setProject] = useState("");
  const [live, setLive] = useState(false);
  const [state, setState] = useState<{
    report: ActivityReport | null;
    runners: readonly FleetRunner[];
    pending: boolean;
    error: string | null;
  }>({ report: null, runners: [], pending: true, error: null });
  useEffect(() => {
    let current = true;
    let request = 0;
    const projects = (bootstrap?.projects ?? [])
      .filter((row) => !project || row.id === project)
      .map((row) => row.id);
    const connected = new Set<string>();
    setLive(false);
    setState((previous) => ({
      ...previous,
      report: null,
      pending: true,
      error: null,
    }));
    const refresh = async () => {
      const version = ++request;
      try {
        const [report, fleet] = await Promise.all([
          api.activity(project ? { project_id: project } : {}),
          api.fleet(),
        ]);
        if (current && request === version)
          setState({
            report,
            runners: fleet.runners,
            pending: false,
            error: null,
          });
      } catch {
        if (current && request === version)
          setState((previous) => ({
            ...previous,
            pending: false,
            error: "Activity is temporarily unavailable.",
          }));
      }
    };
    const refreshVisible = () => {
      if (document.visibilityState === "visible") void refresh();
    };
    const unsubscribe = projects.map((id) =>
      subscribeProjectEvents(http, id, {
        open: () => {
          connected.add(id);
          setLive(connected.size === projects.length);
          refreshVisible();
        },
        activity: () => {
          connected.add(id);
          setLive(connected.size === projects.length);
          refreshVisible();
        },
        error: () => {
          connected.delete(id);
          setLive(false);
        },
      }),
    );
    void refresh();
    document.addEventListener("visibilitychange", refreshVisible);
    return () => {
      current = false;
      unsubscribe.forEach((stop) => stop());
      document.removeEventListener("visibilitychange", refreshVisible);
    };
  }, [api, bootstrap?.projects, http, project]);
  return (
    <ActivityView
      {...state}
      projects={bootstrap?.projects ?? []}
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
  pending,
  error,
  live,
  project,
  onProjectChange,
}: {
  readonly report: ActivityReport | null;
  readonly runners: readonly FleetRunner[];
  readonly projects: readonly AccountProject[];
  readonly pending: boolean;
  readonly error: string | null;
  readonly live: boolean;
  readonly project: string;
  readonly onProjectChange: (value: string) => void;
}) {
  const [runner, setRunner] = useState("");
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
  const scope =
    projects.find((row) => row.id === project)?.name ?? "All projects";
  const selectedRunner = runners.find((row) => row.id === runner);
  const working = runners.filter(
    (row) => runnerCategory(row) === "working",
  ).length;
  const idle = runners.filter((row) => runnerCategory(row) === "idle").length;
  const hosts = fleetHosts(runners);
  const used = hosts.reduce((sum, host) => sum + host.used, 0);
  const slots = hosts.reduce((sum, host) => sum + host.capacity, 0);
  const slowRunners = new Set(
    report?.running
      .filter((row) => timing(row, report, now, false).slow)
      .map((row) => row.runner_id),
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
        {prefix}{" "}
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
  const renderRow = (attempt: ActivityAttempt, finished: boolean) => {
    if (!report) return null;
    const metrics = timing(attempt, report, now, finished);
    const host = runners.find((row) => row.id === attempt.runner_id);
    const prNumber = attempt.pull_request_url?.match(
      /\/pull\/(\d+)(?:[/?#]|$)/,
    )?.[1];
    return (
      <div
        role="row"
        key={attempt.attempt_id}
        className="grid min-w-0 grid-cols-1 gap-2 border-b border-border py-3 md:grid-cols-[minmax(0,1fr)_12rem] md:gap-x-6 md:py-2 @[900px]/activity:grid-cols-[minmax(0,1fr)_8rem_12rem_13.5rem] @[900px]/activity:items-center"
      >
        <div role="cell" className="min-w-0">
          <Tooltip>
            <TooltipTrigger
              render={
                <Link
                  to="/work/i/$workItemId"
                  params={{ workItemId: attempt.work_item_id }}
                  className="block truncate text-sm hover:underline"
                />
              }
            >
              <span className="text-muted-foreground">#{attempt.number}</span>{" "}
              {attempt.title}
            </TooltipTrigger>
            <TooltipPopup>{attempt.title}</TooltipPopup>
          </Tooltip>
          <div className="mt-0.5 flex min-w-0 flex-wrap items-center gap-x-1 text-xs text-muted-foreground">
            <span>{attempt.project_name} ·</span>
            {timeButton(
              finished
                ? (attempt.finished_at ?? attempt.started_at)
                : attempt.started_at,
              finished ? "finished" : "attempt started",
            )}
          </div>
        </div>
        <div
          role="cell"
          className="flex min-w-0 items-center gap-2 text-xs md:col-start-1 md:row-start-2 @[900px]/activity:col-auto @[900px]/activity:row-auto"
        >
          <RunnerStatusDot runner={host} />
          <span
            className="truncate"
            title={host ? runnerName(host) : attempt.runner_id}
          >
            {host ? runnerName(host) : attempt.runner_id}
          </span>
        </div>
        <div
          role="cell"
          className="min-w-0 space-y-1 md:col-start-2 md:row-span-2 md:row-start-1 @[900px]/activity:col-auto @[900px]/activity:row-auto @[900px]/activity:row-span-1"
        >
          <div className="flex items-baseline justify-between gap-2 text-xs">
            <span>
              {metrics.stageLabel}
              {finished && attempt.outcome ? (
                <>
                  {" "}
                  <span
                    className={
                      attempt.outcome === "failed"
                        ? "text-error-foreground"
                        : attempt.outcome === "succeeded"
                          ? "text-success-foreground"
                          : "text-muted-foreground"
                    }
                  >
                    {attempt.outcome}
                  </span>
                </>
              ) : null}
            </span>
            <span
              className={`shrink-0 tabular-nums ${metrics.slow ? "text-warning-foreground" : metrics.fast ? "text-success-foreground" : "text-muted-foreground"}`}
            >
              {metrics.duration}
              {metrics.slow ? " · slow" : metrics.fast ? " · fast" : ""}
            </span>
          </div>
          <StageProgress
            steps={STAGES}
            currentStep={metrics.index}
            progress={metrics.progress}
            tone={metrics.tone}
            label={metrics.label}
            description={metrics.description}
          />
        </div>
        <div
          role="cell"
          className="flex items-center gap-3 text-xs md:col-span-2 @[900px]/activity:col-span-1"
        >
          <span className="w-24 shrink-0">
            {attempt.change_id ? (
              <Link
                to="/work/i/$workItemId/changes/$changeId"
                params={{
                  workItemId: attempt.work_item_id,
                  changeId: attempt.change_id,
                }}
                className="inline-flex items-center gap-1.5 hover:underline"
              >
                <GitPullRequestIcon
                  aria-hidden
                  className="size-3.5 text-muted-foreground"
                />
                {prNumber ? `PR #${prNumber}` : "Change"}
              </Link>
            ) : attempt.pull_request_url ? (
              <a
                href={attempt.pull_request_url}
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
          <span className="w-20 shrink-0">
            {attempt.session_id ? (
              <Link
                to="/work/i/$workItemId"
                params={{ workItemId: attempt.work_item_id }}
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
      finished ? report.finished : report.running,
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
      (row) => timing(row, report, now, finished).slow,
    ).length;
    const title = finished ? "Recent finishes" : "Running now";
    return (
      <section
        aria-label={title}
        className={finished ? "border-t border-border pt-5" : ""}
      >
        <div className="mb-2 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-xs">
          <h2 className="text-sm font-medium text-muted-foreground">{title}</h2>
          <span>
            {rows.length}{" "}
            {finished
              ? rows.length === 1
                ? "attempt"
                : "attempts"
              : rows.length === 1
                ? "job"
                : "jobs"}
          </span>
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
            aria-label={finished ? "Finished attempts" : "Running jobs"}
          >
            <div
              role="row"
              className="hidden grid-cols-[minmax(0,1fr)_8rem_12rem_13.5rem] gap-x-6 border-b border-border pb-1 text-xs text-muted-foreground @[900px]/activity:grid"
            >
              {[
                "Issue",
                "Runner",
                finished ? "Stage and outcome" : "Stage",
                "Links",
              ].map((label) => (
                <span role="columnheader" key={label}>
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
                      {finished
                        ? entry.rows.length === 1
                          ? "attempt"
                          : "attempts"
                        : entry.rows.length === 1
                          ? "job"
                          : "jobs"}
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
          <WorkspaceBreadcrumbItem current={!runner}>
            <span className="truncate">{scope}</span>
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
                    { value: "", label: "All projects" },
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
                        { value: "stage", label: "Stage" },
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
                        { value: "longest", label: "Longest in stage" },
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
                {report?.partial ? (
                  <p className="text-sm text-muted-foreground">
                    Activity is incomplete; some attempts or timing data may be missing.
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
