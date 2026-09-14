// The board's view state, bound to the URL.
//
// The URL is the source of truth and `localStorage` is a fallback used exactly
// once: when a reader arrives at a project's board with no query string at
// all. A pasted link therefore always shows what its sender saw, and a reader
// who comes back to a project they were filtering gets their filter without
// having to reapply it.
import { useNavigate, useRouterState } from "@tanstack/react-router";
import React from "react";

import {
  parseViewState,
  readStoredViewState,
  serializeViewState,
  viewScopeKey,
  writeStoredViewState,
  type WorkViewState,
} from "./viewState.ts";

export function useViewState(
  projectId: string | null,
): readonly [WorkViewState, (next: WorkViewState) => void] {
  const navigate = useNavigate();
  const searchStr = useRouterState({ select: (state) => state.location.searchStr });
  const view = React.useMemo(() => parseViewState(searchStr), [searchStr]);
  const scope = viewScopeKey(projectId);
  const restored = React.useRef<string | null>(null);

  const setView = React.useCallback(
    (next: WorkViewState) => {
      writeStoredViewState(scope, next);
      void navigate({
        // `to: "."` keeps the route and replaces only the query, so changing a
        // filter never re-mounts the board.
        to: ".",
        search: Object.fromEntries(new URLSearchParams(serializeViewState(next))),
        replace: true,
      });
    },
    [navigate, scope],
  );

  // Restore once per scope, and only into an empty query string: a link that
  // carries any parameter at all is the sender's view, not the reader's.
  React.useEffect(() => {
    if (restored.current === scope) return;
    restored.current = scope;
    if (searchStr.replace(/^\?/, "").length > 0) return;
    const stored = readStoredViewState(scope);
    if (stored === null) return;
    void navigate({
      to: ".",
      search: Object.fromEntries(new URLSearchParams(serializeViewState(stored))),
      replace: true,
    });
    // `searchStr` is read, not depended on: a later edit to the query must not
    // re-run the restore and overwrite what the reader just typed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scope, navigate]);

  return [view, setView] as const;
}
