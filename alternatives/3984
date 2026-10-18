// Prefetch on intent: hovering or focusing a destination starts its route
// chunk and its data, so both are usually there by the time it is clicked.
// The shell loads this module on the first intent, so none of it is in the
// entry bundle.
import type { AnyRouter } from "@tanstack/react-router";

import { accountApiFor } from "../account/context.ts";
import { prefetchActivity } from "../activity/activityResource.ts";
import type { useClient } from "../client.ts";
import { prefetchDiagnostics } from "../diagnostics/diagnosticsQuery.ts";
import { prefetchReports } from "../reports/reportsQuery.ts";
import { getBoardRead } from "../work/lib/boardStore.ts";
import { prefetchIssue } from "../work/lib/issueQuery.ts";
import { workHttpFor } from "../work/lib/useWork.ts";
import { DEFAULT_VIEW_STATE, readStoredViewState, viewScopeKey } from "../work/lib/viewState.ts";

export interface PrefetchContext {
  readonly router: AnyRouter;
  readonly client: ReturnType<typeof useClient>;
  readonly activeProjectId: string | null;
}

/** Prefetches the destination at `to`: its route chunk and its first read. */
export function prefetchRoute(to: string, { router, client, activeProjectId }: PrefetchContext): void {
  void router.preloadRoute({ to } as never).catch(() => undefined);
  const path = to.split("?")[0]!;
  const api = accountApiFor(client);
  const http = workHttpFor(client);
  if (path === "/activity") {
    prefetchActivity(api, (client.account?.projects ?? []).map((project) => project.id));
  } else if (path === "/diagnostics") {
    prefetchDiagnostics(api);
  } else if (path === "/reports") {
    if (activeProjectId !== null) prefetchReports(api, activeProjectId);
  } else if (path === "/work" || path.startsWith("/work/p/")) {
    const projectId = path === "/work" ? null : decodeURIComponent(path.slice("/work/p/".length));
    const scope = JSON.stringify([client.http.origin, client.http.apiBase, client.bootstrap.organization.id,
      client.bootstrap.actor?.principal_id, viewScopeKey(projectId)]);
    const board = getBoardRead(client, http, projectId, readStoredViewState(scope) ?? DEFAULT_VIEW_STATE);
    if (!board.snapshot().resolved) board.start();
  } else if (path.startsWith("/work/i/")) {
    prefetchIssue(http, decodeURIComponent(path.slice("/work/i/".length).split("/")[0]!));
  }
}
