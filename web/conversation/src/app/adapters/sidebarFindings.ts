import React from "react";
import type { BootstrapProject } from "../../contracts/index.ts";
import type { HealthFinding } from "../../contracts/diagnostics.ts";
import { scopeOf } from "../lib/queryCache.ts";
import { useHealthFindings } from "../diagnostics/healthFindings.tsx";
import { useWorkHttp } from "../work/lib/useWork.ts";
import { WorkApiError } from "../work/lib/workHttp.ts";
import { runnerDisplay, useRunnerNames } from "../work/lib/runnerNames.ts";
import { reconnectDelayMs } from "../../runtime/state/conversations.ts";

export interface SidebarFinding extends HealthFinding {
  readonly label: string;
  readonly to: string;
}

interface FindingLabel {
  readonly label: Promise<string>;
  readonly attempts: number;
  retryAt: number | null;
}

export function useSidebarFindings(projects: readonly BootstrapProject[]): readonly SidebarFinding[] {
  const http = useWorkHttp();
  const names = useRunnerNames();
  const { read } = useHealthFindings();
  const scope = JSON.stringify([scopeOf(http), projects.map((project) => [project.id, project.name])]);
  const [state, setState] = React.useState<{ scope: string; items: readonly SidebarFinding[] }>({ scope, items: [] });
  const labels = React.useRef<{ scope: string; controller: AbortController; entries: Map<string, FindingLabel> } | null>(null);

  React.useEffect(() => () => {
    labels.current?.controller.abort();
    labels.current = null;
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    if (labels.current?.scope !== scope) {
      labels.current?.controller.abort();
      labels.current = { scope, controller: new AbortController(), entries: new Map() };
    }
    const cache = labels.current;
    const refresh = async () => {
      try {
        const findings = (read?.items ?? []).filter((finding) => finding.severity === "attention" && finding.resolved_at == null);
        const keyOf = (subject: HealthFinding["subject"], openedAt: string) =>
          JSON.stringify([subject.project_id ?? projects[0]?.id ?? "", subject.id, openedAt]);
        const open = new Set(findings.filter((finding) => finding.subject.kind === "work_item").map((finding) => keyOf(finding.subject, finding.opened_at)));
        for (const key of cache.entries.keys()) if (!open.has(key)) cache.entries.delete(key);
        const items = await Promise.all(findings.map(async (finding): Promise<SidebarFinding> => {
          const subject = finding.subject;
          if (subject.kind === "work_item") {
            const key = keyOf(subject, finding.opened_at);
            let entry = cache.entries.get(key);
            if (entry === undefined || (entry.retryAt !== null && Date.now() >= entry.retryAt)) {
              const attempts = (entry?.attempts ?? 0) + 1;
              const next: FindingLabel = { attempts, retryAt: null, label: http.getWorkItem(subject.project_id ?? projects[0]?.id ?? "", subject.id, cache.controller.signal).then((issue) => `#${issue.number} ${issue.title}`, (cause: unknown) => {
                const status = cause instanceof WorkApiError ? cause.status : 0;
                if (status < 400 || status >= 500 || status === 408 || status === 429) next.retryAt = Date.now() + reconnectDelayMs(attempts) * (0.5 + Math.random() * 0.5);
                return subject.id;
              }) };
              entry = next;
              cache.entries.set(key, next);
            }
            return { ...finding, label: await entry.label, to: `/work/i/${encodeURIComponent(subject.id)}?tab=diagnostics` };
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
