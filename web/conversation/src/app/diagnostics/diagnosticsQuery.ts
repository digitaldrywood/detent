import { accountErrorDiagnostic, type AccountApi } from "../account/api.ts";
import { prefetchQuery, scopeOf } from "../lib/queryCache.ts";

export type DiagnosticsWindow = "24h" | "7d" | "30d";

export const DIAGNOSTICS_STALE_MS = 30_000;

export const diagnosticsKey = (api: AccountApi, range: DiagnosticsWindow) => ["diagnostics", scopeOf(api), range] as const;

export const diagnosticsFleetKey = (api: AccountApi) => ["diagnostics-fleet", scopeOf(api)] as const;
export const diagnosticsUpdatesKey = (api: AccountApi) => ["diagnostics-updates", scopeOf(api)] as const;

export function readDiagnostics(api: AccountApi, range: DiagnosticsWindow) {
  return () => api.diagnostics(range);
}

export function readDiagnosticsFleet(api: AccountApi) {
  return async () => {
    try {
      return { runners: (await api.fleet()).runners };
    } catch (error) {
      console.error("Diagnostics fleet read failed", accountErrorDiagnostic(error));
      return { runners: null };
    }
  };
}

export function readDiagnosticsUpdates(api: AccountApi) {
  return () => api.appUpdates().catch(() => null);
}

/** Starts the Diagnostics read for a reader about to open the page. */
export function prefetchDiagnostics(api: AccountApi): void {
  prefetchQuery(diagnosticsFleetKey(api), readDiagnosticsFleet(api), DIAGNOSTICS_STALE_MS);
  prefetchQuery(diagnosticsUpdatesKey(api), readDiagnosticsUpdates(api), DIAGNOSTICS_STALE_MS);
  prefetchQuery(diagnosticsKey(api, "24h"), readDiagnostics(api, "24h"), DIAGNOSTICS_STALE_MS);
}
