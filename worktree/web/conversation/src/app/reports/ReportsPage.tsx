import React from "react";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { DownloadIcon, ListFilterIcon } from "lucide-react";

import { Button } from "../../components/ui/button.tsx";
import { Popover, PopoverPopup, PopoverTrigger } from "../../components/ui/popover.tsx";
import { RefreshIcon } from "../../components/ui/refresh-icon.tsx";
import { ScrollArea } from "../../components/ui/scroll-area.tsx";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "../../components/ui/select.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Tabs, TabsList, TabsPanel, TabsTab } from "../../components/ui/tabs.tsx";
import { Toggle } from "../../components/ui/toggle.tsx";
import { Toggle as GroupToggle, ToggleGroup } from "../../components/ui/toggle-group.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import type { OrganizationReports, ReportsFacets, ReportsReport } from "../../contracts/reports.ts";
import type { BootstrapProject } from "../../contracts/index.ts";
import { useAccountApi } from "../account/context.ts";
import { useSidebarData } from "../adapters/sidebarData.tsx";
import { usePageTitle } from "../pageTitle.ts";
import { formatDateTimeShort, formatDayShort } from "../usage/usageFormat.ts";
import { CostTab, costCsv } from "./CostTab.tsx";
import { FailuresTab } from "./FailuresTab.tsx";
import { anyFilterSet, CustomRangePicker, customRangeLabel, filterOptions, ReportFilterChip } from "./filters.tsx";
import { FlowTab } from "./FlowTab.tsx";
import {
  ALL_PROJECTS,
  CUSTOM_RANGE,
  isReportTab,
  NO_FILTERS,
  parseReportsSearch,
  priorRecorded,
  rangeOption,
  REPORT_FILTERS,
  REPORT_RANGES,
  REPORT_TABS,
  reportsQueryFilters,
  reportsSearchParams,
  type ReportFilterKey,
  type ReportsSearch,
  type ReportTab,
} from "./model.ts";
import { OrganizationOverview } from "./OrganizationOverview.tsx";
import { OverviewTab } from "./OverviewTab.tsx";
import { QualityTab } from "./QualityTab.tsx";
import { useOrganizationReports, useReportOptions, useReports } from "./reportsQuery.ts";

const ISSUE_FILTERS = ["labels", "priorities", "types", "filedBy", "runners"] as const;
/** The chips each tab offers; a chip set on another tab stays visible so it can be cleared. */
const TAB_FILTERS: Readonly<Record<ReportTab, readonly ReportFilterKey[]>> = {
  overview: ISSUE_FILTERS,
  flow: ISSUE_FILTERS,
  quality: ISSUE_FILTERS,
  cost: [...ISSUE_FILTERS, "models", "stages"],
  failures: ISSUE_FILTERS,
};

/** The report with every prior-period value removed when the project did not exist then, so each tile says "No prior data". */
function withPriorPeriod(report: ReportsReport | null): ReportsReport | null {
  if (report === null || priorRecorded(report)) return report;
  return {
    ...report,
    previous: { ...report.previous, partial: true },
    previous_cost_usd: undefined,
    previous_first_try_percent: null,
    previous_failed: undefined,
    prior: undefined,
    open_work: report.open_work === undefined ? undefined : { ...report.open_work, previous_stuck: undefined },
    series: report.series?.map((bucket) => ({ ...bucket, prior_done: undefined })),
  };
}

export function ReportsRoute(): React.ReactElement {
  usePageTitle("Reports");
  const api = useAccountApi();
  const sidebar = useSidebarData();
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as { tab?: string };
  const rawSearch = useSearch({ strict: false }) as Record<string, unknown>;
  const tab: ReportTab = isReportTab(params.tab) ? params.tab : "overview";
  const parsedSearch = parseReportsSearch(rawSearch);
  const orgScope = parsedSearch.project === ALL_PROJECTS || (sidebar !== null && sidebar.activeProjectId === null && parsedSearch.project === null);
  const search = orgScope ? { ...parsedSearch, filters: { ...parsedSearch.filters, models: [], stages: [] } } : parsedSearch;
  const project = sidebar?.projects.find((p) => p.id === (search.project ?? sidebar.activeProjectId));
  const projectId = orgScope ? ALL_PROJECTS : (project?.id ?? null);
  const range = search.range === CUSTOM_RANGE.value && search.from !== null && search.to !== null ? { from: search.from, to: search.to } : search.range === CUSTOM_RANGE.value ? "7d" : search.range;
  const reports = useReports(api, orgScope || projectId === null ? null : { projectId, range, filters: reportsQueryFilters(search.filters) });
  const organization = useOrganizationReports(api, !orgScope || tab !== "overview" ? null : { projectId: ALL_PROJECTS, range, filters: reportsQueryFilters(search.filters), projects: search.projects, compare: search.compare });
  const options = useReportOptions(api, projectId, range);
  React.useEffect(() => {
    const currentSearch = parseReportsSearch(rawSearch);
    if (!orgScope || (rawSearch.project !== undefined && currentSearch.filters.models.length === 0 && currentSearch.filters.stages.length === 0)) return;
    void navigate({
      to: tab === "overview" ? "/reports" : "/reports/$tab",
      params: tab === "overview" ? {} : { tab },
      search: reportsSearchParams({ ...currentSearch, project: ALL_PROJECTS, filters: { ...currentSearch.filters, models: [], stages: [] } }),
      replace: true,
    } as never);
  }, [navigate, orgScope, rawSearch, tab]);
  const go = (nextTab: ReportTab, nextSearch: ReportsSearch) => {
    void navigate({
      to: nextTab === "overview" ? "/reports" : "/reports/$tab",
      params: nextTab === "overview" ? {} : { tab: nextTab },
      search: reportsSearchParams(nextSearch),
    } as never);
  };
  return (
    <ReportsView
      report={orgScope ? null : withPriorPeriod(reports.report)}
      organizationReport={organization.report}
      orgScope={orgScope}
      options={options}
      pending={orgScope ? tab === "overview" && organization.pending : projectId !== null && reports.pending}
      refreshing={orgScope ? organization.refreshing : reports.refreshing}
      error={orgScope ? tab === "overview" ? organization.error : null : reports.error}
      projects={sidebar?.projects ?? []}
      project={project ?? null}
      onProjectChange={sidebar?.onProjectChange}
      tab={tab}
      search={search}
      onNavigate={go}
      onRefresh={() => void (orgScope ? organization.refresh() : reports.refresh())}
    />
  );
}

function downloadCsv(report: ReportsReport, range: string, project: string): void {
  const blob = new Blob([costCsv(report)], { type: "text/csv" });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `${project}-cost-${range}.csv`;
  anchor.click();
  URL.revokeObjectURL(url);
}

const DAY_MS = 86_400_000;

/** "Oct 7 – Oct 13 · 6 days + today · hourly data through Oct 13, 9 AM UTC"; a window ending at midnight ends on the day before. */
export function windowLabel(report: ReportsReport): string {
  const from = Date.parse(report.analytics.window.from);
  const to = Date.parse(report.analytics.window.to);
  const lastDay = new Date(to - 1).toISOString().slice(0, 10);
  const lastHour = new Date(to - 3_600_000).toISOString();
  const days = Math.floor((to - from) / DAY_MS);
  const today = (to - from) % DAY_MS !== 0;
  const span = to - from < 6 * DAY_MS ? "" : ` · ${days} ${days === 1 ? "day" : "days"}${today ? " + today" : ""}`;
  return `${formatDayShort(report.analytics.window.from.slice(0, 10))} – ${formatDayShort(lastDay)}${span} · hourly data through ${formatDateTimeShort(lastHour, "UTC")} UTC`;
}

export function ReportsView({
  report,
  organizationReport = null,
  orgScope = false,
  options,
  pending,
  refreshing,
  error,
  projects,
  project,
  onProjectChange,
  tab,
  search,
  onNavigate,
  onRefresh,
}: {
  readonly report: ReportsReport | null;
  readonly organizationReport?: OrganizationReports | null;
  readonly orgScope?: boolean;
  readonly options?: ReportsFacets | undefined;
  readonly pending: boolean;
  readonly refreshing: boolean;
  readonly error: string | null;
  readonly projects: readonly BootstrapProject[];
  readonly project: BootstrapProject | null;
  readonly onProjectChange?: ((id: string) => void) | undefined;
  readonly tab: ReportTab;
  readonly search: ReportsSearch;
  readonly onNavigate: (tab: ReportTab, search: ReportsSearch) => void;
  readonly onRefresh: () => void;
}): React.ReactElement {
  const range = rangeOption(search.range);
  const setSearch = (patch: Partial<ReportsSearch>) => onNavigate(tab, { ...search, ...patch });
  const setRange = (value: unknown) => {
    const match = REPORT_RANGES.find((option) => option.value === value);
    if (match) setSearch({ range: match.value, from: null, to: null });
  };
  const setCustom = (from: string, to: string) => setSearch({ range: CUSTOM_RANGE.value, from, to });
  const custom = search.range === CUSTOM_RANGE.value && search.from !== null && search.to !== null ? customRangeLabel(search.from, search.to) : null;
  const allProjects = orgScope || search.project === ALL_PROJECTS;
  const projectName = allProjects ? "All projects" : (project?.name ?? "Choose a project");
  const rangeName = custom === null ? search.range : `${search.from}-${search.to}`;
  const chips = [
    ...(orgScope && tab === "overview" ? [
      <ReportFilterChip
        key="projects"
        label="Project"
        options={projects.map((item) => ({ value: item.id, label: item.name }))}
        selected={search.projects}
        onChange={(values) => setSearch({ projects: values })}
      />,
    ] : []),
    ...REPORT_FILTERS.filter((filter) => TAB_FILTERS[tab].includes(filter.key) || search.filters[filter.key].length > 0).map((filter) => (
      <ReportFilterChip
        key={filter.key}
        label={filter.label}
        options={filterOptions(filter.key, options, search.filters[filter.key])}
        selected={search.filters[filter.key]}
        onChange={(values) => setSearch({ filters: { ...search.filters, [filter.key]: values } })}
      />
    )),
  ];
  const setCount = Object.values(search.filters).filter((values) => values.length > 0).length + (orgScope && search.projects.length > 0 ? 1 : 0);
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-background text-foreground">
      <WorkspacePageHeader className="h-auto min-h-(--workspace-topbar-height) border-border border-b">
        <div className="flex w-full min-w-0 flex-wrap items-center gap-2 py-2">
          <WorkspaceBreadcrumb ariaLabel="Reports breadcrumb" className="min-w-0 flex-1">
            <WorkspaceBreadcrumbItem>
              <h1>Reports</h1>
            </WorkspaceBreadcrumbItem>
            <WorkspaceBreadcrumbSeparator />
            <WorkspaceBreadcrumbItem current>
              {projects.length > 0 && onProjectChange ? (
                <Select
                  value={allProjects ? ALL_PROJECTS : (project?.id ?? "")}
                  onValueChange={(value) => {
                    if (typeof value !== "string" || value === "") return;
                    if (value === ALL_PROJECTS) {
                      setSearch({ project: ALL_PROJECTS });
                      return;
                    }
                    setSearch({ project: value, projects: [] });
                    onProjectChange(value);
                  }}
                >
                  <SelectTrigger aria-label="Project" size="compact" className="w-auto min-w-0 max-w-56">
                    <SelectValue>{projectName}</SelectValue>
                  </SelectTrigger>
                  <SelectPopup align="start" alignItemWithTrigger={false}>
                    <SelectItem value={ALL_PROJECTS}>All projects</SelectItem>
                    {projects.map((item) => (
                      <SelectItem key={item.id} value={item.id}>
                        {item.name}
                      </SelectItem>
                    ))}
                  </SelectPopup>
                </Select>
              ) : (
                <span className="truncate">{projectName}</span>
              )}
            </WorkspaceBreadcrumbItem>
          </WorkspaceBreadcrumb>
          <div className="hidden items-center gap-2 sm:flex">
            <ToggleGroup aria-label="Report range" value={[search.range]} onValueChange={(value) => setRange(value[0])}>
              {REPORT_RANGES.map((option) => (
                <GroupToggle key={option.value} value={option.value}>
                  {option.label}
                </GroupToggle>
              ))}
              <CustomRangePicker
                from={search.from}
                to={search.to}
                onApply={setCustom}
                trigger={
                  <GroupToggle value={CUSTOM_RANGE.value} aria-label={custom === null ? "Custom range" : `Custom range, ${custom}`}>
                    {custom ?? CUSTOM_RANGE.label}
                  </GroupToggle>
                }
              />
            </ToggleGroup>
            <Toggle size="sm" variant="outline" pressed={search.compare} onPressedChange={(pressed) => setSearch({ compare: pressed })}>
              vs {range.prior}
            </Toggle>
            {tab === "cost" ? (
              <Button
                size="compact"
                variant="outline"
                disabled={!report}
                onClick={() => report && downloadCsv(report, rangeName, allProjects ? "all-projects" : (project?.name ?? "project"))}
              >
                <DownloadIcon aria-hidden />
                Export CSV
              </Button>
            ) : null}
            <Button size="icon-sm" variant="ghost" aria-label="Refresh reports" disabled={pending || refreshing} onClick={onRefresh}>
              <RefreshIcon refreshing={pending || refreshing} />
            </Button>
          </div>
        </div>
      </WorkspacePageHeader>
      <Tabs value={tab} onValueChange={(value) => isReportTab(value) && onNavigate(value, search)} className="min-h-0 flex-1">
        <div className={`flex items-center gap-2 ps-(--workspace-gutter-start) pe-(--workspace-gutter-end) pt-3 sm:hidden ${orgScope && tab === "overview" ? "flex-wrap" : ""}`}>
          <Select value={search.range} onValueChange={setRange}>
            <SelectTrigger aria-label="Report range" className="w-auto min-w-0">
              <SelectValue>{custom ?? range.long}</SelectValue>
            </SelectTrigger>
            <SelectPopup align="start" alignItemWithTrigger={false}>
              {REPORT_RANGES.map((option) => (
                <SelectItem key={option.value} value={option.value}>
                  {option.long}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
          <CustomRangePicker
            from={search.from}
            to={search.to}
            onApply={setCustom}
            trigger={<Button variant="outline" aria-label="Custom range">{CUSTOM_RANGE.label}</Button>}
          />
          <Popover>
            <PopoverTrigger render={<Button variant="outline" aria-label={setCount > 0 ? `Filters, ${setCount} set` : "Filters"} />}>
              <ListFilterIcon aria-hidden />
              {setCount > 0 ? `Filters · ${setCount}` : "Filters"}
            </PopoverTrigger>
            <PopoverPopup align="start" width="sm">
              <div role="group" aria-label="Filters" className="flex flex-wrap items-center gap-2 pointer-coarse:gap-4">
                {chips}
              </div>
              {anyFilterSet(search.filters) || search.projects.length > 0 ? (
                <Button size="compact" variant="ghost" className="mt-2" onClick={() => setSearch({ filters: NO_FILTERS, projects: [] })}>
                  Clear filters
                </Button>
              ) : null}
            </PopoverPopup>
          </Popover>
          {orgScope && tab === "overview" ? <Toggle size="sm" variant="outline" pressed={search.compare} onPressedChange={(pressed) => setSearch({ compare: pressed })}>vs {range.prior}</Toggle> : null}
          <Button size="icon" variant="ghost" aria-label="Refresh reports" disabled={pending || refreshing} onClick={onRefresh} className="ml-auto">
            <RefreshIcon refreshing={pending || refreshing} />
          </Button>
        </div>
        <TabsList aria-label="Report sections" className="ps-(--workspace-gutter-start) pe-(--workspace-gutter-end)">
          {REPORT_TABS.map((item) => (
            <TabsTab key={item.id} value={item.id}>
              {item.label}
            </TabsTab>
          ))}
        </TabsList>
        <ScrollArea className="min-h-0 flex-1">
          <WorkspacePageContainer width="expanded" data-testid="reports-page" className="pt-3">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <div role="group" aria-label="Filters" className="hidden flex-wrap items-center gap-2 pointer-coarse:gap-4 sm:flex">
                {chips}
              </div>
              <span className="flex-1" />
              {tab === "flow" ? (
                <ToggleGroup
                  aria-label="Clock starts"
                  value={[search.clock]}
                  onValueChange={(value) => {
                    if (value[0] === "todo" || value[0] === "created") setSearch({ clock: value[0] });
                  }}
                >
                  <GroupToggle value="todo">From first Todo</GroupToggle>
                  <GroupToggle value="created">From created</GroupToggle>
                </ToggleGroup>
              ) : tab === "failures" ? (
                <ToggleGroup
                  aria-label="Failure kind"
                  value={[search.kind]}
                  onValueChange={(value) => {
                    if (value[0] === "all" || value[0] === "infrastructure" || value[0] === "issue") setSearch({ kind: value[0] });
                  }}
                >
                  <GroupToggle value="all">All</GroupToggle>
                  <GroupToggle value="infrastructure">Infrastructure</GroupToggle>
                  <GroupToggle value="issue">Issue</GroupToggle>
                </ToggleGroup>
              ) : report && tab === "overview" ? (
                <span className="text-muted-foreground text-xs">{windowLabel(report)}</span>
              ) : null}
            </div>
            <TabsPanel value={tab} className="flex flex-col gap-6">
              {pending ? (
                <div role="status" aria-label="Loading reports" className="flex flex-col gap-6">
                  <Skeleton className="h-24 w-full rounded-xl" />
                  <Skeleton className="h-64 w-full rounded-xl" />
                  <Skeleton className="h-44 w-full rounded-xl" />
                </div>
              ) : error ? (
                <p role="status" className="text-muted-foreground text-sm">
                  {error}
                </p>
              ) : orgScope && tab === "overview" && organizationReport ? (
                <OrganizationOverview report={organizationReport} search={search} projects={projects} options={options} onSearch={setSearch} onProjectChange={onProjectChange} />
              ) : !report ? (
                <p className="text-muted-foreground text-xs">Choose a project in the sidebar to view its reports.</p>
              ) : tab === "flow" ? (
                <FlowTab report={report} search={search} />
              ) : tab === "quality" ? (
                <QualityTab report={report} search={search} />
              ) : tab === "cost" ? (
                <CostTab report={report} search={search} onSearch={setSearch} />
              ) : tab === "failures" ? (
                <FailuresTab report={report} search={search} />
              ) : (
                <OverviewTab report={report} search={search} />
              )}
            </TabsPanel>
          </WorkspacePageContainer>
        </ScrollArea>
      </Tabs>
    </div>
  );
}
