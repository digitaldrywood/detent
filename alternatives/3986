import type { AccountProject, FleetRunner } from "../../contracts/account.ts";

const PROBLEMS: Record<string, { summary: string; impact: string; action: string; projectSettings?: boolean }> = {
  policy_mismatch: { summary: "Project policy differs from its approval", impact: "This runner cannot start this project's work until its policy matches the approval.", action: "Review policy", projectSettings: true },
  settings_invalid: { summary: "Project settings could not be loaded", impact: "This runner cannot prepare work with these settings.", action: "Runner operator: validate this project's detent.yaml and WORKFLOW.md in the runner's logs." },
  settings_rejected: { summary: "Runner settings could not be applied", impact: "The requested routing settings have not taken effect.", action: "Runner operator: check routing cache permissions and configured host services." },
  tier_unavailable: { summary: "Required isolation is unavailable", impact: "Work requiring this isolation cannot start on this runner.", action: "Runner operator: repair the configured sandbox tooling." },
  backend_probe_failed: { summary: "Agent backend readiness check did not complete", impact: "Work using this backend may not start on this runner until the check passes.", action: "Runner operator: check the backend's probe in the runner's logs and let its next heartbeat refresh readiness." },
  backend_missing: { summary: "Configured agent backend is unavailable", impact: "Work using this backend cannot start on this runner.", action: "Runner operator: install the configured backend and verify its command runs." },
  host_service_unreachable: { summary: "Required host service is unreachable", impact: "Work requiring this service cannot start on this runner.", action: "Runner operator: start the configured host service and check its socket or port." },
  keep_awake_failed: { summary: "Runner cannot keep the host awake", impact: "Host sleep may interrupt current work.", action: "Runner operator: repair the host sleep inhibitor and its permissions." },
  version_unsupported: { summary: "Runner version is unsupported", impact: "This runner cannot take new work from this Hub.", action: "Runner operator: upgrade Detent to the Hub's supported version." },
};

export function runnerFindings(runners: readonly FleetRunner[], projects: readonly AccountProject[], canManage: boolean) {
  const rows = new Map<string, {
    id: string;
    runners: FleetRunner[];
    project: string;
    summary: string;
    impact: string;
    action: string;
    destination: string | null;
  }>();
  for (const runner of runners) {
    const connection = runner.connection_health ?? runner.health;
    if (["revoked", "expired"].includes(connection)) continue;
    const stopped = [connection === "offline" || connection === "unknown" ? "offline" : "", runner.state !== "active" ? runner.state : ""].filter(Boolean);
    for (const problem of runner.problems ?? []) {
      const id = JSON.stringify([problem.code, problem.project_id, problem.subject, problem.check, stopped]);
      const existing = rows.get(id);
      if (existing) {
        if (!existing.runners.some((entry) => entry.id === runner.id)) existing.runners.push(runner);
        continue;
      }
      const project = projects.find((entry) => entry.id === problem.project_id);
      const content = PROBLEMS[problem.code] ?? { summary: "Runner check needs attention", impact: "This runner may be unable to prepare work.", action: "Runner operator: inspect the failed local check in the runner's logs." };
      const destination = content.projectSettings && project && canManage ? `/settings/integrations?project=${encodeURIComponent(project.id)}` : null;
      rows.set(id, {
        id, runners: [runner], project: project?.name ?? (problem.project_id ? "Project unavailable to you" : "Assigned projects"),
        summary: stopped.length > 0 ? `Not taking work because ${content.summary.charAt(0).toLowerCase()}${content.summary.slice(1)}` : content.summary,
        impact: stopped.length > 0 ? `This runner is ${stopped.join(" and ")}. ${content.impact}` : content.impact,
        action: content.projectSettings && !destination ? "Organization administrator: review the project's approved policy in Settings → Integrations." : content.action,
        destination,
      });
    }
  }
  return [...rows.values()];
}
