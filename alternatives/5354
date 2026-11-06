import type { AccountApi } from "../account/api.ts";
import { prefetchQuery, scopeOf, useQuery } from "../lib/queryCache.ts";
import type { OrganizationReports, ReportsCustomRange, ReportsFacets, ReportsFilters, ReportsRange, ReportsReport } from "../../contracts/reports.ts";

export const REPORTS_STALE_MS = 60_000;
export const DEFAULT_REPORTS_RANGE: ReportsRange = "7d";

/** One Reports read: a project or `all`, a range, and the filters the Hub applies. */
export interface ReportsView {
  readonly projectId: string;
  readonly range: ReportsRange | ReportsCustomRange;
  readonly filters?: ReportsFilters | undefined;
}

/** The set filters only, so an unfiltered view shares its cache entry with the prefetch. */
const filtersKey = (filters: ReportsFilters | undefined) =>
  JSON.stringify(Object.entries(filters ?? {}).filter(([, values]) => values.length > 0).toSorted(([a], [b]) => a.localeCompare(b)));

const reportsKey = (api: AccountApi, view: ReportsView) =>
  ["reports", scopeOf(api), view.projectId, typeof view.range === "string" ? view.range : `${view.range.from}..${view.range.to}`, filtersKey(view.filters)] as const;

const readReports = (api: AccountApi, view: ReportsView) => api.reports(view.projectId, view.range, view.filters);

/** Starts the Reports read for a reader about to open the page. */
export function prefetchReports(api: AccountApi, projectId: string, range: ReportsRange = DEFAULT_REPORTS_RANGE): void {
  const view = { projectId, range };
  prefetchQuery(reportsKey(api, view), () => readReports(api, view), REPORTS_STALE_MS);
}

export interface ReportsRead {
  readonly report: ReportsReport | null;
  /** True only on a first load: nothing cached for this project yet. */
  readonly pending: boolean;
  readonly refreshing: boolean;
  readonly error: string | null;
  readonly refresh: () => Promise<void>;
}

/**
 * One report for a view, through the client cache: a revisit paints the
 * cached report, and a range or filter change keeps the last report on screen
 * until the new one arrives.
 */
export function useReports(api: AccountApi, view: ReportsView | null): ReportsRead {
  const query = useQuery(
    view === null ? null : reportsKey(api, view),
    () => readReports(api, view!),
    { staleTime: REPORTS_STALE_MS, keepPreviousIn: JSON.stringify([scopeOf(api), view?.projectId ?? null]) },
  );
  return {
    report: query.error === null ? query.data ?? null : null,
    pending: query.pending,
    refreshing: query.refreshing,
    error: query.error !== null ? "Reports are temporarily unavailable." : null,
    refresh: query.refresh,
  };
}

/**
 * The filter chips' choices for a project or `all` over a range, read apart
 * from the report: a failed read leaves the chips with only their fixed and
 * selected values and never touches the report.
 */
export function useReportOptions(api: AccountApi, projectId: string | null, range: ReportsRange | ReportsCustomRange): ReportsFacets | undefined {
  const rangeKey = typeof range === "string" ? range : `${range.from}..${range.to}`;
  const query = useQuery(
    projectId === null ? null : (["report-options", scopeOf(api), projectId, rangeKey] as const),
    () => api.reportOptions(projectId!, range),
    { staleTime: REPORTS_STALE_MS, keepPreviousIn: JSON.stringify([scopeOf(api), projectId]) },
  );
  return query.data ?? undefined;
}

export function useOrganizationReports(api: AccountApi, view: ReportsView & { readonly filters: ReportsFilters; readonly projects: readonly string[]; readonly compare: boolean } | null) {
  const query = useQuery<OrganizationReports>(
    view === null ? null : ["organization-reports", scopeOf(api), typeof view.range === "string" ? view.range : `${view.range.from}..${view.range.to}`, filtersKey(view.filters), JSON.stringify(view.projects), String(view.compare)],
    () => api.organizationReports(view!.range, view!.filters, view!.projects, view!.compare),
    { staleTime: REPORTS_STALE_MS, keepPreviousIn: JSON.stringify([scopeOf(api), view === null]) },
  );
  return {
    report: query.error === null ? query.data ?? null : null,
    pending: query.pending,
    refreshing: query.refreshing,
    error: query.error !== null ? "Reports are temporarily unavailable." : null,
    refresh: query.refresh,
  };
}
