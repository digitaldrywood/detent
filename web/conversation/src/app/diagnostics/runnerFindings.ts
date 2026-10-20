import type { AccountProject, FleetRunner } from "../../contracts/account.ts";

const PROBLEMS: Record<string, { summary: string; impact: string; action: string }> = {
  settings_invalid: { summary: "Project settings could not be loaded", impact: "This runner cannot prepare work with these settings.", action: "Runner operator: validate this project's detent.yaml and WORKFLOW.md in the runner's logs." },
  settings_rejected: { summary: "Runner settings could not be applied", impact: "The requested routing settings have not taken effect.", action: "Runner operator: check routing cache permissions and configured host services." },
  tier_unavailable: { summary: "Required isolation is unavailable", impact: "Work requiring this isolation cannot start on this runner.", action: "Runner operator: repair the configured sandbox tooling." },
  backend_missing: { summary: "Configured agent backend is unavailable", impact: "Work using this backend cannot start on this runner.", action: "Runner operator: install the configured backend and verify its command runs." },
  host_service_unreachable: { summary: "Required host service is unreachable", impact: "Work requiring this service cannot start on this runner.", action: "Runner operator: start the configured host service and check its socket or port." },
  keep_awake_failed: { summary: "Runner cannot keep the host awake", impact: "Host sleep may interrupt current work.", action: "Runner operator: repair the host sleep inhibitor and its permissions." },
  version_unsupported: { summary: "Runner version is unsupported", impact: "This runner cannot take new work from this Hub.", action: "Runner operator: upgrade Detent to the Hub's supported version." },
};

export function runnerFindings(runners: readonly FleetRunner[], projects: readonly AccountProject[]) {
  const rows = new Map<string, {
    id: string;
    runners: FleetRunner[];
    project: string;
    summary: string;
    impact: string;
    action: string;
  }>();
  for (const runner of runners) {
    if (runner.state !== "active" && runner.leases.length === 0) continue;
    const connection = runner.connection_health ?? runner.health;
    if (["offline", "unknown", "revoked", "expired"].includes(connection)) continue;
    for (const problem of runner.problems ?? []) {
      const id = JSON.stringify([problem.code, problem.project_id, problem.subject, problem.check]);
      const existing = rows.get(id);
      if (existing) {
        if (!existing.runners.some((entry) => entry.id === runner.id)) existing.runners.push(runner);
        continue;
      }
      const project = projects.find((entry) => entry.id === problem.project_id);
      const content = PROBLEMS[problem.code] ?? { summary: "Runner check needs attention", impact: "This runner may be unable to prepare work.", action: "Runner operator: inspect the failed local check in the runner's logs." };
      rows.set(id, {
        id, runners: [runner], project: project?.name ?? (problem.project_id ? "Project unavailable to you" : "Assigned projects"),
        summary: content.summary, impact: content.impact,
        action: content.action,
      });
    }
  }
  return [...rows.values()];
}
