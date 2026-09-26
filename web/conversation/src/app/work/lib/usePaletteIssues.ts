// The issues the command palette can open.
//
// `useBoard` is the board's own read: it fans out over every project, follows
// cursors and enriches each issue with its attempts and change request. The
// palette needs none of that — a row is a number, a title and a lane — and it
// must not pay for a board load on a route that never shows one. So this is a
// single `listWorkItems` page for the project in scope, run only while the
// palette's dialog is mounted, which is only while it is open.
import React from "react";

import { useWorkHttp } from "./useWork.ts";

export interface PaletteIssue {
  readonly id: string;
  readonly identifier: string;
  readonly title: string;
  readonly lane: string;
}

/** One page is the whole reach: past it, the board itself is the right surface. */
const PALETTE_ISSUE_LIMIT = 100;

export function useWorkIssueItems(projectId: string | null): readonly PaletteIssue[] {
  const http = useWorkHttp();
  const [issues, setIssues] = React.useState<readonly PaletteIssue[]>([]);

  React.useEffect(() => {
    if (projectId === null || projectId === "") {
      setIssues([]);
      return;
    }
    let cancelled = false;
    void http
      .listWorkItems({ projectId, limit: PALETTE_ISSUE_LIMIT })
      .then((page) => {
        if (cancelled) return;
        setIssues(
          page.items.map((item) => ({
            id: item.work_item_id,
            identifier: `#${item.number}`,
            title: item.title,
            lane: item.state,
          })),
        );
      })
      .catch(() => {
        // A palette that cannot list issues still opens everything else.
        if (!cancelled) setIssues([]);
      });
    return () => {
      cancelled = true;
    };
  }, [http, projectId]);

  return issues;
}
