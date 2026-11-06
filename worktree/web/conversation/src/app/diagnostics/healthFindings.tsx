import React from "react";
import type { BootstrapProject } from "../../contracts/index.ts";
import type { HealthFinding, HealthFindingsRead } from "../../contracts/diagnostics.ts";
import { scopeOf } from "../lib/queryCache.ts";
import { observeProjectQuery } from "../work/lib/projectEvents.ts";
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
    const byProject = new Map<string, HealthFindingsRead>();
    const failed = new Set<string>();
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
    const queries = projects.map((project) => {
      const query = observeProjectQuery(http, project.id, {
        topics: ["health.findings"],
        load: async (signal) => {
          const items: HealthFinding[] = [];
          let cursor: string | undefined;
          let lastTick: string | null = null;
          let watermark: number | undefined;
          do {
            const page = await http.listHealthFindings(project.id, cursor, signal);
            items.push(...page.items.map((finding) => ({ ...finding, subject: { ...finding.subject, project_id: finding.subject.project_id ?? project.id } })));
            lastTick = page.last_tick_at;
            watermark = Math.min(watermark ?? Infinity, page.change_sequence ?? 0);
            cursor = page.next_cursor || undefined;
          } while (cursor !== undefined && !signal.aborted);
          return { value: { items, last_tick_at: lastTick }, watermark };
        },
        apply: (read) => { byProject.set(project.id, read); failed.delete(project.id); publish(); },
        error: () => { failed.add(project.id); publish(); },
        event: () => query.refresh(),
      });
      return query;
    });
    refreshRef.current = () => queries.forEach((query) => query.refresh());
    return () => { queries.forEach((query) => query.close()); refreshRef.current = () => {}; };
  }, [http, scope]);
  const refresh = React.useCallback(() => refreshRef.current(), []);
  const value = React.useMemo(() => ({ ...(state.scope === scope ? state.value : EMPTY), refresh }), [state, scope, refresh]);
  return <FindingsContext.Provider value={value}>{children}</FindingsContext.Provider>;
}
