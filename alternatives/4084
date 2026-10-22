import type { AccountApi } from "../account/api.ts";
import { prefetchQuery, scopeOf } from "../lib/queryCache.ts";

export type DiagnosticsWindow = "24h" | "7d" | "30d";

export const DIAGNOSTICS_STALE_MS = 30_000;

export const diagnosticsKey = (api: AccountApi, range: DiagnosticsWindow) => ["diagnostics", scopeOf(api), range] as const;

export function readDiagnostics(api: AccountApi, range: DiagnosticsWindow) {
  return async () => {
    const [report, fleet, updates] = await Promise.allSettled([api.diagnostics(range), api.fleet(), api.appUpdates()]);
    if (report.status === "rejected") throw report.reason;
    return { report: report.value, runners: fleet.status === "fulfilled" ? fleet.value.runners : null, appUpdates: updates.status === "fulfilled" ? updates.value : null };
  };
}

/** Starts the Diagnostics read for a reader about to open the page. */
export function prefetchDiagnostics(api: AccountApi): void {
  prefetchQuery(diagnosticsKey(api, "24h"), readDiagnostics(api, "24h"), DIAGNOSTICS_STALE_MS);
}
