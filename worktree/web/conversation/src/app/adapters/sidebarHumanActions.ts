import React from "react";
import type { BootstrapProject } from "../../contracts/index.ts";
import type { IssueExplanation } from "../../contracts/diagnostics.ts";
import type { NativeIssue } from "../../contracts/work.ts";
import { scopeOf } from "../lib/queryCache.ts";
import { subscribeProjectEvents } from "../work/lib/projectEvents.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";

export interface SidebarHumanAction {
  readonly id: string;
  readonly label: string;
  readonly to: string;
  readonly opened_at: string;
}

export function pendingHumanAction(issue: NativeIssue, explanation: IssueExplanation): SidebarHumanAction | null {
  if (issue.terminal || issue.archived || explanation.current_lane.name !== issue.state) return null;
  if (!["blocked", "human review", "plan review"].includes(issue.state.toLowerCase())) return null;
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
    const controller = new AbortController();
    const byProject = new Map<string, readonly SidebarHumanAction[]>();
    const revisions = new Map<string, number>();
    const refresh = async (project: BootstrapProject) => {
      const revision = (revisions.get(project.id) ?? 0) + 1;
      revisions.set(project.id, revision);
      try {
        const workflow = await http.getProject(project.id, controller.signal);
        const holding = workflow.states.filter((lane) => !lane.terminal && !lane.dispatchable &&
          ["blocked", "human review", "plan review"].includes(lane.name.toLowerCase())).map((lane) => lane.name);
        const items: SidebarHumanAction[] = [];
        let cursor: string | undefined;
        if (holding.length > 0) do {
          const page = await http.listWorkItems({ projectId: project.id, state: holding, open: true, cursor, signal: controller.signal });
          const actions = await Promise.all(page.items.map(async (issue) => {
            try {
              return pendingHumanAction(issue, await http.getExplanation(project.id, issue.work_item_id));
            } catch {
              return null;
            }
          }));
          items.push(...actions.filter((action): action is SidebarHumanAction => action !== null));
          cursor = page.next_cursor || undefined;
        } while (cursor !== undefined && !controller.signal.aborted);
        if (controller.signal.aborted || revisions.get(project.id) !== revision) return;
        byProject.set(project.id, items);
      } catch {
        if (controller.signal.aborted || revisions.get(project.id) !== revision) return;
        byProject.delete(project.id);
      }
      setState({ scope, items: [...byProject.values()].flat().sort((a, b) => Date.parse(a.opened_at) - Date.parse(b.opened_at) || a.id.localeCompare(b.id)) });
    };
    const unsubscribe = projects.filter((project) => project.can_write).map((project) => {
      void refresh(project);
      return subscribeProjectEvents(http, project.id, { activity: () => { void refresh(project); } });
    });
    return () => { controller.abort(); unsubscribe.forEach((stop) => stop()); };
  }, [http, scope]);
  return state.scope === scope ? state.items : [];
}
