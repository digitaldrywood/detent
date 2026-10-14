import React from "react";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { DownloadIcon, ListFilterIcon } from "lucide-react";

import { Button } from "../../components/ui/button.tsx";
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
import type { ReportsReport, ReportsRange } from "../../contracts/reports.ts";
import type { BootstrapProject } from "../../contracts/index.ts";
import { useAccountApi } from "../account/context.ts";
import { useSidebarData } from "../adapters/sidebarData.tsx";
import { usePageTitle } from "../pageTitle.ts";
import { formatDateTimeShort, formatDayShort } from "../usage/usageFormat.ts";
import { CostTab, costCsv } from "./CostTab.tsx";
import { FailuresTab } from "./FailuresTab.tsx";
import { FlowTab } from "./FlowTab.tsx";
import {
  isReportTab,
  parseReportsSearch,
  rangeOption,
  REPORT_RANGES,
  REPORT_TABS,
  reportsSearchParams,
  type ReportsSearch,
  type ReportTab,
} from "./model.ts";
import { OverviewTab } from "./OverviewTab.tsx";
import { QualityTab } from "./QualityTab.tsx";
import { useReports } from "./reportsQuery.ts";

const FILTERS = ["Label", "Priority", "Type", "Filed by"] as const;
const TAB_FILTERS: Readonly<Record<ReportTab, readonly string[]>> = {
  overview: [...FILTERS, "Runner"],
  flow: [...FILTERS, "Runner"],
  quality: [...FILTERS, "Runner"],
  cost: [...FILTERS, "Model", "Stage"],
  failures: ["Label", "Priority", "Runner", "Stage"],
};

export function ReportsRoute(): React.ReactElement {
  usePageTitle("Reports");
  const api = useAccountApi();
  const sidebar = useSidebarData();
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as { tab?: string };
  const rawSearch = useSearch({ strict: false }) as Record<string, unknown>;
  const tab: ReportTab = isReportTab(params.tab) ? params.tab : "overview";
  const search = parseReportsSearch(rawSearch);
  const project = sidebar?.projects.find((p) => p.id === sidebar.activeProjectId);
  const reports = useReports(api, project?.id ?? null, search.range);
  const go = (nextTab: ReportTab, nextSearch: ReportsSearch) => {
    void navigate({
      to: nextTab === "overview" ? "/reports" : "/reports/$tab",
      params: nextTab === "overview" ? {} : { tab: nextTab },
      search: reportsSearchParams(nextSearch),
    } as never);
  };
  return (
    <ReportsView
      report={reports.report}
      pending={project !== undefined && reports.pending}
      error={reports.error}
      projects={sidebar?.projects ?? []}
      project={project ?? null}
      onProjectChange={sidebar?.onProjectChange}
      tab={tab}
      search={search}
      onNavigate={go}
      onRefresh={() => void reports.refresh()}
    />
  );
}

function downloadCsv(report: ReportsReport, range: ReportsRange, project: string): void {
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
  pending,
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
  readonly pending: boolean;
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
    if (match) setSearch({ range: match.value });
  };
  const filters = TAB_FILTERS[tab];
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
                <Select value={project?.id ?? ""} onValueChange={(value) => typeof value === "string" && value !== "" && onProjectChange(value)}>
                  <SelectTrigger aria-label="Project" size="compact" className="w-auto min-w-0 max-w-56">
                    <SelectValue>{project?.name ?? "Choose a project"}</SelectValue>
                  </SelectTrigger>
                  <SelectPopup align="start" alignItemWithTrigger={false}>
                    {projects.map((item) => (
                      <SelectItem key={item.id} value={item.id}>
                        {item.name}
                      </SelectItem>
                    ))}
                  </SelectPopup>
                </Select>
              ) : (
                <span className="truncate">{project?.name ?? "Choose a project"}</span>
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
              <GroupToggle value="custom" disabled aria-label="Custom range: not available yet">
                Custom
              </GroupToggle>
            </ToggleGroup>
            <Toggle size="sm" variant="outline" pressed={search.compare} onPressedChange={(pressed) => setSearch({ compare: pressed })}>
              vs {range.prior}
            </Toggle>
            {tab === "cost" ? (
              <Button
                size="compact"
                variant="outline"
                disabled={!report}
                onClick={() => report && downloadCsv(report, search.range, project?.name ?? "project")}
              >
                <DownloadIcon aria-hidden />
                Export CSV
              </Button>
            ) : null}
            <Button size="icon-sm" variant="ghost" aria-label="Refresh reports" disabled={pending} onClick={onRefresh}>
              <RefreshIcon refreshing={pending} />
            </Button>
          </div>
        </div>
      </WorkspacePageHeader>
      <Tabs value={tab} onValueChange={(value) => isReportTab(value) && onNavigate(value, search)} className="min-h-0 flex-1">
        <div className="flex items-center gap-2 ps-(--workspace-gutter-start) pe-(--workspace-gutter-end) pt-3 sm:hidden">
          <Select value={search.range} onValueChange={setRange}>
            <SelectTrigger aria-label="Report range" className="w-auto min-w-0">
              <SelectValue>{range.long}</SelectValue>
            </SelectTrigger>
            <SelectPopup align="start" alignItemWithTrigger={false}>
              {REPORT_RANGES.map((option) => (
                <SelectItem key={option.value} value={option.value}>
                  {option.long}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
          <Button variant="outline" disabled aria-label="Filters: not available yet">
            <ListFilterIcon aria-hidden />
            Filters
          </Button>
          <Button size="icon" variant="ghost" aria-label="Refresh reports" disabled={pending} onClick={onRefresh} className="ml-auto">
            <RefreshIcon refreshing={pending} />
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
              <div role="group" aria-label="Filters" className="hidden flex-wrap items-center gap-2 sm:flex">
                {filters.map((name) => (
                  <Toggle key={name} size="compact" variant="pill-outline" disabled aria-label={`${name} filter: not available yet`}>
                    {name}
                  </Toggle>
                ))}
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
