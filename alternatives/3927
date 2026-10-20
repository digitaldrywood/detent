import React from "react";
import type { BootstrapProject } from "../../contracts/index.ts";
import type { HealthFinding } from "../../contracts/diagnostics.ts";
import { scopeOf } from "../lib/queryCache.ts";
import { useHealthFindings } from "../diagnostics/healthFindings.tsx";
import { useWorkHttp } from "../work/lib/useWork.ts";
import { runnerDisplay, useRunnerNames } from "../work/lib/runnerNames.ts";

export interface SidebarFinding extends HealthFinding {
  readonly label: string;
  readonly to: string;
}

export function useSidebarFindings(projects: readonly BootstrapProject[]): readonly SidebarFinding[] {
  const http = useWorkHttp();
  const names = useRunnerNames();
  const { read } = useHealthFindings();
  const scope = JSON.stringify([scopeOf(http), projects.map((project) => [project.id, project.name])]);
  const [state, setState] = React.useState<{ scope: string; items: readonly SidebarFinding[] }>({ scope, items: [] });

  React.useEffect(() => {
    const controller = new AbortController();
    const refresh = async () => {
      try {
        const findings = (read?.items ?? []).filter((finding) => finding.severity === "attention" && finding.resolved_at == null);
        const subjects = new Map<string, Promise<string>>();
        const items = await Promise.all(findings.map(async (finding): Promise<SidebarFinding> => {
          const subject = finding.subject;
          if (subject.kind === "work_item") {
            let label = subjects.get(subject.id);
            if (label === undefined) {
              label = http.getWorkItem(subject.project_id ?? projects[0]?.id ?? "", subject.id).then((issue) => `#${issue.number} ${issue.title}`).catch(() => subject.id);
              subjects.set(subject.id, label);
            }
            return { ...finding, label: await label, to: `/work/i/${encodeURIComponent(subject.id)}?tab=diagnostics` };
          }
          if (subject.kind === "runner") return { ...finding, label: subject.id, to: "/fleet" };
          return { ...finding, label: projects.find((candidate) => candidate.id === subject.id)?.name ?? projects.find((candidate) => candidate.id === subject.project_id)?.name ?? subject.id, to: "/diagnostics" };
        }));
        if (controller.signal.aborted) return;
        setState({ scope, items: items.sort((a, b) => Date.parse(a.opened_at) - Date.parse(b.opened_at) || a.id.localeCompare(b.id)) });
      } catch {
        if (!controller.signal.aborted) setState({ scope, items: [] });
      }
    };
    void refresh();
    return () => controller.abort();
  }, [http, scope, read]);

  return React.useMemo(() => state.scope === scope ? state.items.map((finding) => finding.subject.kind === "runner"
    ? { ...finding, label: runnerDisplay(names, finding.subject.id) ?? finding.subject.id }
    : finding) : [], [state, scope, names]);
}
