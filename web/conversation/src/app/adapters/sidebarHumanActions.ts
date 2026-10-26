import React from "react";
import * as Schema from "effect/Schema";
import type { BootstrapProject } from "../../contracts/index.ts";
import type { IssueExplanation } from "../../contracts/diagnostics.ts";
import { NativeBoardFrame, type NativeIssue } from "../../contracts/work.ts";
import { fetchQuery, getQuerySnapshot, invalidateQueries, scopeOf } from "../lib/queryCache.ts";
import { subscribeProjectEvents } from "../work/lib/projectEvents.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";

const refreshInterval = 30_000;
const workflowStaleTime = 120_000;
const relevantDelay = 2000;
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
    const controller = new AbortController();
    const byProject = new Map<string, readonly SidebarHumanAction[]>();
    const explanations = new Map<string, { readonly key: string; readonly action: SidebarHumanAction | null }>();
    const running = new Set<string>();
    const pending = new Set<string>();
    const notBefore = new Map<string, number>();
    const lastRefresh = new Map<string, number>();
    const timers = new Map<string, ReturnType<typeof setTimeout>>();
    const writable = projects.filter((project) => project.can_write);
    const canRead = (project: BootstrapProject): boolean => {
      if (controller.signal.aborted) return false;
      if (document.visibilityState !== "hidden") return true;
      pending.add(project.id);
      return false;
    };
    const clearTimer = (id: string) => {
      clearTimeout(timers.get(id));
      timers.delete(id);
    };
    const schedule = (project: BootstrapProject, prompt = false) => {
      pending.add(project.id);
      if (prompt && !notBefore.has(project.id)) notBefore.set(project.id, Date.now() + relevantDelay);
      if (!canRead(project) || running.has(project.id)) return;
      const delay = Math.max(0, (lastRefresh.get(project.id) ?? -Infinity) + refreshInterval - Date.now(),
        (notBefore.get(project.id) ?? -Infinity) - Date.now());
      if (delay === 0) {
        clearTimer(project.id);
        void refresh(project);
      } else if (!timers.has(project.id)) {
        timers.set(project.id, setTimeout(() => {
          timers.delete(project.id);
          schedule(project);
        }, delay));
      }
    };
    const actionFor = async (project: BootstrapProject, issue: NativeIssue): Promise<SidebarHumanAction | null> => {
      const key = `${issue.revision}|${issue.last_activity_at ?? ""}|${issue.updated_at}|${issue.state}`;
      const cached = explanations.get(issue.work_item_id);
      if (cached?.key === key) return cached.action;
      const action = pendingHumanAction(issue, await http.getExplanation(project.id, issue.work_item_id));
      explanations.set(issue.work_item_id, { key, action });
      return action;
    };
    const refresh = async (project: BootstrapProject): Promise<void> => {
      if (!canRead(project)) return;
      pending.delete(project.id);
      notBefore.delete(project.id);
      lastRefresh.set(project.id, Date.now());
      running.add(project.id);
      try {
        await load(project);
      } finally {
        running.delete(project.id);
        if (pending.has(project.id) && !controller.signal.aborted) schedule(project);
      }
    };
    const load = async (project: BootstrapProject) => {
      try {
        const workflow = await fetchQuery(["sidebar-workflow", scopeOf(http), project.id],
          (signal) => http.getProject(project.id, signal), { staleTime: workflowStaleTime });
        if (!canRead(project)) return;
        if (workflow === undefined) throw new Error("Project workflow unavailable");
        const holding = workflow.states.filter((lane) => !lane.terminal && !lane.dispatchable &&
          holdingLanes.has(lane.name.toLowerCase())).map((lane) => lane.name);
        const items: SidebarHumanAction[] = [];
        let cursor: string | undefined;
        if (holding.length > 0) do {
          const page = await http.listWorkItems({ projectId: project.id, state: holding, open: true, cursor, signal: controller.signal });
          if (!canRead(project)) return;
          const actions = await Promise.all(page.items.map(async (issue) => {
            try {
              return await actionFor(project, issue);
            } catch {
              return null;
            }
          }));
          items.push(...actions.filter((action): action is SidebarHumanAction => action !== null));
          cursor = page.next_cursor || undefined;
          if (!canRead(project)) return;
        } while (cursor !== undefined);
        if (controller.signal.aborted) return;
        byProject.set(project.id, items);
      } catch {
        if (controller.signal.aborted) return;
        byProject.delete(project.id);
      }
      setState({ scope, items: [...byProject.values()].flat().sort((a, b) => Date.parse(a.opened_at) - Date.parse(b.opened_at) || a.id.localeCompare(b.id)) });
    };
    const invalidateWorkflow = (project: BootstrapProject) => {
      invalidateQueries(["sidebar-workflow", scopeOf(http), project.id]);
      schedule(project);
    };
    const onActivity = (project: BootstrapProject, event: Event) => {
      let frame: NativeBoardFrame;
      try {
        const data: unknown = JSON.parse((event as MessageEvent<string>).data);
        if (typeof data === "number") {
          invalidateWorkflow(project);
          return;
        }
        frame = Schema.decodeUnknownSync(NativeBoardFrame)(data);
      } catch {
        invalidateWorkflow(project);
        return;
      }
      const relevant = frame.deltas.some(({ previous, current }) => {
        const before = previous?.issue;
        const after = current?.issue;
        const transition = before?.state !== after?.state &&
          [before, after].some((issue) => issue !== undefined && holdingLanes.has(issue.state.toLowerCase()));
        return transition || (byProject.get(project.id) ?? []).some((action) =>
          action.id === before?.work_item_id || action.id === after?.work_item_id);
      });
      if (frame.gap) invalidateWorkflow(project);
      else if (relevant) schedule(project, true);
      else if (Date.now() - getQuerySnapshot(["sidebar-workflow", scopeOf(http), project.id]).updatedAt >= workflowStaleTime) schedule(project);
    };
    const unsubscribe = writable.map((project) => {
      let subscribing = true;
      const stop = subscribeProjectEvents(http, project.id, { activity: (event) => {
        if (!subscribing) onActivity(project, event);
      }, open: () => invalidateWorkflow(project) });
      subscribing = false;
      schedule(project);
      return stop;
    });
    const onVisibility = () => {
      for (const project of writable) {
        if (document.visibilityState === "hidden") {
          clearTimer(project.id);
          pending.add(project.id);
        } else if (pending.has(project.id)) {
          schedule(project);
        }
      }
    };
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      controller.abort();
      document.removeEventListener("visibilitychange", onVisibility);
      timers.forEach(clearTimeout);
      unsubscribe.forEach((stop) => stop());
    };
  }, [http, scope]);
  return state.scope === scope ? state.items : [];
}
