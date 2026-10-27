import React from "react";
import type { BootstrapProject } from "../../contracts/index.ts";
import type { IssueExplanation } from "../../contracts/diagnostics.ts";
import type { NativeIssue } from "../../contracts/work.ts";
import { fetchQuery, invalidateQueries, scopeOf } from "../lib/queryCache.ts";
import { observeProjectQuery, projectEventData } from "../work/lib/projectEvents.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";

const workflowStaleTime = 120_000;
const holdingLanes = new Set(["blocked", "human review", "plan review"]);

export interface SidebarHumanAction {
  readonly id: string;
  readonly label: string;
  readonly to: string;
  readonly opened_at: string;
}

export function pendingHumanAction(issue: NativeIssue, explanation: IssueExplanation): SidebarHumanAction | null {
  if (issue.terminal || issue.archived || explanation.current_lane.name !== issue.state) return null;
  if (!holdingLanes.has(issue.state.toLowerCase())) return null;
  const disposition = explanation.native_runtime?.attempt?.disposition;
  const humanAction = issue.issue_contract?.human_action?.trim() || explanation.required_gate.human_action?.trim() ||
    (disposition?.human_action && disposition.status.toLowerCase() === issue.state.toLowerCase());
  if (!humanAction) return null;
  return {
    id: issue.work_item_id,
    label: `#${issue.number} ${issue.title}`,
    to: `/work/i/${encodeURIComponent(issue.work_item_id)}`,
    opened_at: explanation.latest_transition?.at ?? issue.updated_at,
  };
}

export function useSidebarHumanActions(projects: readonly BootstrapProject[]): readonly SidebarHumanAction[] {
  const http = useWorkHttp();
  const scope = JSON.stringify([scopeOf(http), projects.filter((project) => project.can_write).map((project) => project.id)]);
  const [state, setState] = React.useState<{ scope: string; items: readonly SidebarHumanAction[] }>({ scope, items: [] });
  React.useEffect(() => {
    const byProject = new Map<string, readonly SidebarHumanAction[]>();
    const publish = () => setState({ scope, items: [...byProject.values()].flat().sort((a, b) =>
      Date.parse(a.opened_at) - Date.parse(b.opened_at) || a.id.localeCompare(b.id)) });
    const queries = projects.filter((project) => project.can_write).map((project) => {
      const explanations = new Map<string, { key: string; value: SidebarHumanAction | null }>();
      const query = observeProjectQuery(http, project.id, {
        topics: ["human_action", "work_item", "board", "lane"],
        beforeRestart: () => {
          invalidateQueries(["sidebar-workflow", scopeOf(http), project.id]);
          explanations.clear();
        },
        load: async (signal) => {
          const workflow = await fetchQuery(["sidebar-workflow", scopeOf(http), project.id],
            (readSignal) => http.getProject(project.id, readSignal), { staleTime: workflowStaleTime });
          if (workflow === undefined) throw new Error("Project workflow unavailable");
          const holding = workflow.states.filter((lane) => !lane.terminal && !lane.dispatchable &&
            holdingLanes.has(lane.name.toLowerCase())).map((lane) => lane.name);
          const items: SidebarHumanAction[] = [];
          let cursor: string | undefined;
          let watermark: number | undefined;
          do {
            const page = await http.listWorkItems({ projectId: project.id, state: holding, open: true, cursor, signal,
              ...(holding.length === 0 ? { limit: 1 } : {}) });
            watermark = Math.min(watermark ?? Infinity, page.change_sequence ?? 0);
            const actions = await Promise.all((holding.length === 0 ? [] : page.items).map(async (issue) => {
              const key = `${issue.revision}|${issue.last_activity_at ?? ""}|${issue.updated_at}|${issue.state}`;
              const cached = explanations.get(issue.work_item_id);
              if (cached?.key === key) return cached.value;
              try {
                const explanation = await http.getExplanation(project.id, issue.work_item_id);
                signal.throwIfAborted();
                const value = pendingHumanAction(issue, explanation);
                explanations.set(issue.work_item_id, { key, value });
                return value;
              } catch { return null; }
            }));
            items.push(...actions.filter((action): action is SidebarHumanAction => action !== null));
            cursor = holding.length === 0 ? undefined : page.next_cursor || undefined;
          } while (cursor !== undefined && !signal.aborted);
          return { value: items, watermark };
        },
        apply: (items) => { byProject.set(project.id, items); publish(); },
        error: () => { byProject.delete(project.id); publish(); },
        event: (event) => {
          if (event.type === "human_action") {
            const data = projectEventData(event);
            explanations.delete(String(data.related_id || data.entity_id));
          }
          if (event.type === "lane") invalidateQueries(["sidebar-workflow", scopeOf(http), project.id]);
          query.refresh();
        },
      });
      return query;
    });
    return () => queries.forEach((query) => query.close());
  }, [http, scope]);
  return state.scope === scope ? state.items : [];
}
