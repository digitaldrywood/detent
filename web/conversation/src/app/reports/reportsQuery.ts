import type { AccountApi } from "../account/api.ts";
import { prefetchQuery, scopeOf, useQuery } from "../lib/queryCache.ts";
import type { ReportsRange, ReportsReport } from "../../contracts/reports.ts";

export const REPORTS_STALE_MS = 60_000;
export const DEFAULT_REPORTS_RANGE: ReportsRange = "48h";

const reportsKey = (api: AccountApi, projectId: string, range: ReportsRange) => ["reports", scopeOf(api), projectId, range] as const;

/** Starts the Reports read for a reader about to open the page. */
export function prefetchReports(api: AccountApi, projectId: string, range: ReportsRange = DEFAULT_REPORTS_RANGE): void {
  prefetchQuery(reportsKey(api, projectId, range), () => api.reports(projectId, range), REPORTS_STALE_MS);
}

export interface ReportsRead {
  readonly report: ReportsReport | null;
  /** True only on a first load: nothing cached for this project yet. */
  readonly pending: boolean;
  readonly error: string | null;
  readonly refresh: () => Promise<void>;
}

/**
 * One project's report for a range, through the client cache: a revisit
 * paints the cached report, and a range change keeps the last report on
 * screen until the new one arrives.
 */
export function useReports(api: AccountApi, projectId: string | null, range: ReportsRange): ReportsRead {
  const query = useQuery(
    projectId === null ? null : reportsKey(api, projectId, range),
    () => api.reports(projectId!, range),
    { staleTime: REPORTS_STALE_MS, keepPreviousIn: JSON.stringify([scopeOf(api), projectId]) },
  );
  return {
    report: query.data ?? null,
    pending: query.pending,
    error: query.error !== null && query.data === undefined ? "Reports are temporarily unavailable." : null,
    refresh: query.refresh,
  };
}
