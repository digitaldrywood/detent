import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import type { BoardPage } from "../lib/useWork.ts";
import type { WorkPage } from "../lib/viewState.ts";

export function WorkPagination({ pages, loading, onChange }: {
  pages: readonly BoardPage[];
  loading: boolean;
  onChange: (projectId: string, page: WorkPage | undefined) => void;
}): React.ReactElement {
  return (
    <nav aria-label="Work pages" className="flex flex-wrap items-center gap-3 px-5 pb-3 text-xs">
      <span className="w-full text-muted-foreground">
        Search, sorting and multi-value filters apply to the loaded pages and open selection. Filter choices come from loaded items. Lane totals use project scope with server filters.
      </span>
      {pages.map((page) => (
        <div key={page.projectId} className="flex items-center gap-2" data-testid={`work-page-${page.projectId}`}>
          <span>{page.projectName} · Page {page.number}</span>
          <Button size="xs" variant="outline" disabled={loading || page.number === 1}
            aria-label={`${page.projectName}: First page`} onClick={() => onChange(page.projectId, undefined)}>
            First page
          </Button>
          <Button size="xs" variant="outline" disabled={loading || page.previous === undefined}
            aria-label={`${page.projectName}: Previous page`} onClick={() => onChange(page.projectId, page.previous ?? undefined)}>
            Previous
          </Button>
          <Button size="xs" variant="outline" disabled={loading || page.nextCursor === undefined}
            aria-label={`${page.projectName}: Next page`} onClick={() => {
              if (page.nextCursor === undefined) return;
              onChange(page.projectId, { cursor: page.nextCursor, number: page.number + 1 });
            }}>
            Next
          </Button>
        </div>
      ))}
    </nav>
  );
}
