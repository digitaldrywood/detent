import { useNavigate, useRouterState } from "@tanstack/react-router";
import React from "react";

import { useClient } from "../../client.ts";
import { useWorkHttp } from "./useWork.ts";
import {
  DEFAULT_VIEW_STATE,
  parseViewState,
  readStoredViewState,
  serializeViewState,
  viewScopeKey,
  writeStoredViewState,
  type WorkViewState,
} from "./viewState.ts";

function viewSearch(state: WorkViewState): Record<string, unknown> {
  return {
    ...Object.fromEntries(new URLSearchParams(serializeViewState(state))),
    ...(state.archived === true ? { archived: true } : {}),
  };
}

export function useViewState(
  projectId: string | null,
): readonly [WorkViewState, (next: WorkViewState) => void] {
  const navigate = useNavigate();
  const http = useWorkHttp();
  const client = useClient();
  const searchStr = useRouterState({ select: (state) => state.location.searchStr });
  const search = React.useRef(searchStr);
  search.current = searchStr;
  const scope = JSON.stringify([client.http.origin, client.http.apiBase, client.bootstrap.organization.id,
    client.bootstrap.actor?.principal_id, viewScopeKey(projectId)]);
  const cached = React.useMemo(() => readStoredViewState(scope) ?? DEFAULT_VIEW_STATE, [scope]);
  const [resolved, setResolved] = React.useState({ scope, view: cached });
  const session = React.useRef<{ scope: string; changed: boolean; save: (next: WorkViewState) => void } | null>(null);
  const writes = React.useRef<Promise<unknown>>(Promise.resolve());
  const fallback = resolved.scope === scope ? resolved.view : cached;
  const view = searchStr.replace(/^\?/, "").length > 0 ? parseViewState(searchStr) : fallback;

  React.useEffect(() => {
    const controller = new AbortController();
    const initialSearch = search.current;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let pending: WorkViewState | null = null;
    const flush = () => {
      if (pending === null) return;
      const next = pending;
      pending = null;
      const query = serializeViewState(next);
      writes.current = writes.current.catch(() => undefined).then(async () => {
        const confirmed = await http.setViewPreference(projectId, query === "" ? null : query);
        writeStoredViewState(scope, parseViewState(confirmed.query ?? ""));
      }).catch(() => undefined);
    };
    const current = { scope, changed: false, save: (next: WorkViewState) => {
      pending = next;
      clearTimeout(timer);
      timer = setTimeout(flush, 400);
    } };
    session.current = current;
    globalThis.addEventListener?.("pagehide", flush);
    void http.getViewPreference(projectId, controller.signal).then((preference) => {
      if (controller.signal.aborted || current.changed) return;
      const restored = parseViewState(preference.query ?? "");
      writeStoredViewState(scope, restored);
      setResolved({ scope, view: restored });
      if (initialSearch.replace(/^\?/, "").length > 0 || search.current !== initialSearch) return;
      void navigate({ to: ".", search: viewSearch(restored), replace: true });
    }).catch(() => undefined);
    return () => {
      globalThis.removeEventListener?.("pagehide", flush);
      controller.abort();
      clearTimeout(timer);
      flush();
    };
  }, [http, navigate, projectId, scope]);

  const setView = React.useCallback((next: WorkViewState) => {
    const current = session.current;
    if (current?.scope === scope) {
      current.changed = true;
      current.save(next);
    }
    setResolved({ scope, view: next });
    void navigate({ to: ".", search: viewSearch(next), replace: true });
  }, [navigate, scope]);

  return [view, setView] as const;
}
