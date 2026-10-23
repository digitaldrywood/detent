import React from "react";
import type { BootstrapProject } from "../../contracts/index.ts";
import type { HealthFinding, HealthFindingsRead } from "../../contracts/diagnostics.ts";
import { scopeOf } from "../lib/queryCache.ts";
import { subscribeProjectEvents } from "../work/lib/projectEvents.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";

interface FindingsState {
  readonly read: HealthFindingsRead | null;
  readonly error: boolean;
}

const EMPTY: FindingsState = { read: null, error: false };
const FindingsContext = React.createContext({ ...EMPTY, refresh: () => {} });

export function useHealthFindings() {
  return React.useContext(FindingsContext);
}

export function HealthFindingsProvider({ projects, children }: { projects: readonly BootstrapProject[]; children: React.ReactNode }) {
  const http = useWorkHttp();
  const scope = JSON.stringify([scopeOf(http), projects.map((project) => project.id)]);
  const [state, setState] = React.useState({ scope, value: EMPTY });
  const refreshRef = React.useRef(() => {});
  React.useEffect(() => {
    const controller = new AbortController();
    const byProject = new Map<string, HealthFindingsRead>();
    const revisions = new Map<string, number>();
    const since = new Map<string, string>();
    const failed = new Set<string>();
    let tick: string | null = null;
    let pendingTick: string | null = null;
    let refreshingTick: string | null = null;
    const publish = () => {
      const reads = [...byProject.values()];
      const ticks = reads.map((read) => read.last_tick_at).filter((at): at is string => at !== null);
      setState({ scope, value: {
        read: {
          items: [...new Map(reads.flatMap((read) => read.items).map((finding) => [finding.id, finding])).values()],
          last_tick_at: ticks.length === projects.length ? ticks.toSorted((a, b) => Date.parse(a) - Date.parse(b))[0] ?? null : null,
        },
        error: failed.size > 0,
      } });
    };
    const refresh = async (project: BootstrapProject) => {
      const revision = (revisions.get(project.id) ?? 0) + 1;
      revisions.set(project.id, revision);
      try {
        const items: HealthFinding[] = [];
        let cursor: string | undefined;
        let lastTick: string | null = null;
        const from = since.get(project.id);
        do {
          const page = await http.listHealthFindings(project.id, cursor, controller.signal, from === undefined ? undefined : { state: "all", since: from });
          items.push(...page.items.map((finding) => ({ ...finding, subject: { ...finding.subject, project_id: finding.subject.project_id ?? project.id } })));
          lastTick = page.last_tick_at;
          cursor = page.next_cursor || undefined;
        } while (cursor !== undefined && !controller.signal.aborted);
        if (controller.signal.aborted || revisions.get(project.id) !== revision) return;
        if (lastTick !== null) since.set(project.id, lastTick);
        byProject.set(project.id, { items, last_tick_at: lastTick });
        failed.delete(project.id);
      } catch {
        if (controller.signal.aborted || revisions.get(project.id) !== revision) return;
        failed.add(project.id);
      }
    };
    let batch = 0;
    const refreshAll = async (eventTick?: string) => {
      const version = ++batch;
      await Promise.all(projects.map(refresh));
      if (controller.signal.aborted || batch !== version) return;
      if (failed.size === 0) tick = eventTick ?? tick ?? [...byProject.values()][0]?.last_tick_at ?? null;
      publish();
    };
    let initialized = false;
    const initialRefresh = refreshAll().then(() => { initialized = true; });
    refreshRef.current = () => { void refreshAll(); };
    const refreshTick = () => {
      if (document.visibilityState === "hidden" || pendingTick === null || pendingTick === tick || pendingTick === refreshingTick) return;
      const eventTick = pendingTick;
      refreshingTick = eventTick;
      void refreshAll(eventTick).finally(() => { if (refreshingTick === eventTick) refreshingTick = null; });
    };
    const project = projects[0];
    const unsubscribe = project === undefined ? undefined : subscribeProjectEvents(http, project.id, {
      "health.findings": ((event: MessageEvent<string>) => {
        pendingTick = event.data;
        if (initialized) refreshTick();
        else void initialRefresh.then(() => { if (!controller.signal.aborted) refreshTick(); });
      }) as EventListener,
    });
    document.addEventListener("visibilitychange", refreshTick);
    return () => {
      controller.abort();
      unsubscribe?.();
      document.removeEventListener("visibilitychange", refreshTick);
      refreshRef.current = () => {};
    };
  }, [http, scope]);
  const refresh = React.useCallback(() => refreshRef.current(), []);
  const value = React.useMemo(() => ({ ...(state.scope === scope ? state.value : EMPTY), refresh }), [state, scope, refresh]);
  return <FindingsContext.Provider value={value}>{children}</FindingsContext.Provider>;
}
