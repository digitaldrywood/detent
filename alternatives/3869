import React from "react";

import type { WorkItemPullRequest } from "../../contracts/work.ts";
import type { PullRequestState } from "../../contracts/ui.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import type { WorkHttp } from "../work/lib/workHttp.ts";
import type { GitStatusPullRequest } from "./gitStatus.ts";

export interface IssuePullRequest {
  /** Null until the connector has seen a pull request for the change. */
  readonly number: number | null;
  /** The host's page for it. Empty when there is no pull request yet. */
  readonly url: string;
  /** The head branch, when the connector reported one. */
  readonly branch: string | null;
  readonly state: string;
  readonly draft: boolean;
}

/** Open first, then most recently updated. */
function rank(pullRequest: WorkItemPullRequest): number {
  return pullRequest.state === "open" ? 0 : 1;
}

export function selectIssuePullRequest(
  rows: readonly WorkItemPullRequest[],
): IssuePullRequest | null {
  const usable = rows.filter((row) => row.number > 0 || row.head.ref.trim().length > 0);
  if (usable.length === 0) return null;
  const best = [...usable].sort((a, b) => {
    const byState = rank(a) - rank(b);
    if (byState !== 0) return byState;
    return b.updated_at.localeCompare(a.updated_at);
  })[0] as WorkItemPullRequest;
  const branch = best.head.ref.trim();
  return {
    number: best.number > 0 ? best.number : null,
    url: best.url,
    branch: branch.length === 0 ? null : branch,
    state: best.state,
    draft: best.draft,
  };
}

async function read(
  http: WorkHttp,
  projectId: string,
  workItemId: string,
): Promise<IssuePullRequest | null> {
  return selectIssuePullRequest(await http.listPullRequests(projectId, workItemId));
}

/**
 * The linked issue's pull request, or null while there is none, the project
 * has no GitHub connector, or the read failed.
 *
 * A failure is null rather than an error surface: this feeds two chips under
 * the composer, and a chat whose pull request cannot be read is not a chat the
 * reader should be interrupted about. The right panel's Pull request surface
 * is where that story is told.
 */
export function useIssuePullRequest(
  projectId: string | null,
  workItemId: string | null,
): IssuePullRequest | null {
  const http = useWorkHttp();
  const [pullRequest, setPullRequest] = React.useState<IssuePullRequest | null>(null);

  React.useEffect(() => {
    if (projectId === null || workItemId === null) {
      setPullRequest(null);
      return;
    }
    let cancelled = false;
    void read(http, projectId, workItemId)
      .then((next) => {
        if (!cancelled) setPullRequest(next);
      })
      .catch(() => {
        if (!cancelled) setPullRequest(null);
      });
    return () => {
      cancelled = true;
    };
  }, [http, projectId, workItemId]);

  return pullRequest;
}

/** The GitHub host a §18.6 connector's rows point at. */
function connectorBaseUrl(row: WorkItemPullRequest): string {
  try {
    return new URL(row.url).origin;
  } catch {

    return "https://github.com";
  }
}

function toState(state: string): PullRequestState {
  if (state === "merged") return "merged";
  if (state === "closed") return "closed";
  return "open";
}

export interface IssuePullRequestView {

  readonly pullRequest: GitStatusPullRequest | null;

  readonly connector: { readonly baseUrl: string } | null;
}

const NO_VIEW: IssuePullRequestView = { pullRequest: null, connector: null };

/** Both projections of the §18.6 rows, from the selection rule above. */
export function selectIssuePullRequestView(
  rows: readonly WorkItemPullRequest[],
): IssuePullRequestView {
  const withConnector = rows.find((row) => (row.connector ?? null) !== null);
  const connector =
    withConnector === undefined ? null : { baseUrl: connectorBaseUrl(withConnector) };
  const usable = rows.filter((row) => row.number > 0);
  if (usable.length === 0) return { pullRequest: null, connector };
  const best = [...usable].sort((a, b) => {
    const byState = rank(a) - rank(b);
    if (byState !== 0) return byState;
    return b.updated_at.localeCompare(a.updated_at);
  })[0] as WorkItemPullRequest;
  return {
    connector,
    pullRequest: {
      number: best.number,
      title: best.title,
      url: best.url,
      baseRef: best.base.ref,
      headRef: best.head.ref,
      state: toState(best.state),
      isDraft: best.draft,
      updatedAt: best.updated_at,
    },
  };
}

/**
 * The §18.6 rows for the header's git group.
 *
 * A failure is `NO_VIEW` rather than an error surface, for the same reason the
 * hook above returns null: this feeds a disabled reason on a menu row, and the
 * right panel's Pull request surface is where a failed read is a story.
 */
export function useIssuePullRequestView(
  projectId: string | null,
  workItemId: string | null,
): IssuePullRequestView {
  const http = useWorkHttp();
  const [view, setView] = React.useState<IssuePullRequestView>(NO_VIEW);

  React.useEffect(() => {
    if (projectId === null || workItemId === null) {
      setView(NO_VIEW);
      return;
    }
    let cancelled = false;
    void http
      .listPullRequests(projectId, workItemId)
      .then((rows) => {
        if (!cancelled) setView(selectIssuePullRequestView(rows));
      })
      .catch(() => {
        if (!cancelled) setView(NO_VIEW);
      });
    return () => {
      cancelled = true;
    };
  }, [http, projectId, workItemId]);

  return view;
}
