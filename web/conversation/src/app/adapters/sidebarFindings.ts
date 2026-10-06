import React from "react";
import type { BootstrapProject } from "../../contracts/index.ts";
import type { HealthFinding } from "../../contracts/diagnostics.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import { runnerDisplay, useRunnerNames } from "../work/lib/runnerNames.ts";

export interface SidebarFinding extends HealthFinding {
  readonly label: string;
  readonly to: string;
}

export function useSidebarFindings(projects: readonly BootstrapProject[]): readonly SidebarFinding[] {
  const http = useWorkHttp();
  const names = useRunnerNames();
  const scope = JSON.stringify(projects.map((project) => [project.id, project.name]));
  const [state, setState] = React.useState<{ scope: string; items: readonly SidebarFinding[] }>({ scope, items: [] });

  React.useEffect(() => {
    const controller = new AbortController();
    const byProject = new Map<string, readonly SidebarFinding[]>();
    const revisions = new Map<string, number>();
    const sources: EventSource[] = [];
    const publish = () => {
      const items = new Map<string, SidebarFinding>();
      for (const findings of byProject.values()) {
        for (const finding of findings) items.set(finding.id, finding);
      }
      setState({ scope, items: [...items.values()].sort((a, b) => Date.parse(a.opened_at) - Date.parse(b.opened_at) || a.id.localeCompare(b.id)) });
    };
    const refresh = async (project: BootstrapProject) => {
      const revision = (revisions.get(project.id) ?? 0) + 1;
      revisions.set(project.id, revision);
      try {
        const findings: HealthFinding[] = [];
        let cursor: string | undefined;
        do {
          const page = await http.listHealthFindings(project.id, cursor, controller.signal);
          findings.push(...page.items.filter((finding) => finding.severity === "attention" && finding.resolved_at == null));
          cursor = page.next_cursor || undefined;
        } while (cursor !== undefined && !controller.signal.aborted);
        const subjects = new Map<string, Promise<string>>();
        const items = await Promise.all(findings.map(async (finding): Promise<SidebarFinding> => {
          const subject = finding.subject;
          if (subject.kind === "work_item") {
            let label = subjects.get(subject.id);
            if (label === undefined) {
              label = http.getWorkItem(project.id, subject.id).then((issue) => `#${issue.number} ${issue.title}`).catch(() => subject.id);
              subjects.set(subject.id, label);
            }
            return { ...finding, label: await label, to: `/work/i/${encodeURIComponent(subject.id)}?tab=diagnostics` };
          }
          if (subject.kind === "runner") return { ...finding, label: runnerDisplay(names, subject.id) ?? subject.id, to: "/fleet" };
          return { ...finding, label: projects.find((candidate) => candidate.id === subject.id)?.name ?? project.name, to: "/diagnostics" };
        }));
        if (controller.signal.aborted || revisions.get(project.id) !== revision) return;
        byProject.set(project.id, items);
        publish();
      } catch {
        if (controller.signal.aborted || revisions.get(project.id) !== revision) return;
        byProject.delete(project.id);
        publish();
      }
    };
    for (const project of projects) {
      void refresh(project);
      if (typeof globalThis.EventSource !== "function") continue;
      const source = new globalThis.EventSource(http.eventsUrl(project.id), { withCredentials: true });
      let tick: string | null = null;
      source.addEventListener("health.findings", ((event: MessageEvent<string>) => {
        if (tick === event.data) return;
        tick = event.data;
        void refresh(project);
      }) as EventListener);
      source.addEventListener("open", () => void refresh(project));
      sources.push(source);
    }
    return () => {
      controller.abort();
      for (const source of sources) source.close();
    };
  }, [http, scope, names]);

  return state.scope === scope ? state.items : [];
}
