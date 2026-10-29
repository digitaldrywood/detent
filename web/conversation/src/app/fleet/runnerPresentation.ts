import type { FleetRunner } from "../../contracts/account.ts";

export function runnerUpdateFailed(runner: FleetRunner): boolean {
  return runner.update?.status === "refused" || runner.update?.status === "uncertain";
}

export function runnerNeedsAttention(runner: FleetRunner): boolean {
  return runner.health === "needs_attention" || runnerUpdateFailed(runner);
}

export function runnerProblemLabel(problem: NonNullable<FleetRunner["problems"]>[number]): string {
  const labels: Record<string, string> = {
    backend_probe_failed: "Backend probe failed",
    host_service_unreachable: "Host service unavailable",
    keep_awake_failed: "Could not keep this runner awake",
    settings_rejected: "Runner settings rejected",
    version_unsupported: "Runner update needed",
    backend_missing: "Provider sign-in or setup needed",
    backend_probe_timeout: "Backend probe timed out",
    tier_unavailable: "Agent access unavailable",
    policy_mismatch: "Project policy needs approval",
    settings_invalid: "Runner configuration needs attention",
    checkout_missing: "Project checkout unavailable",
  };
  return labels[problem.code] ?? "Runner needs attention";
}

export function runnerReason(runner: FleetRunner): string | undefined {
  if (runnerUpdateFailed(runner)) return "Update failed · reinstall needed";
  if (runner.claim_refusal_reason) return "Runner update needed";
  if (runner.problems?.length) return runnerProblemLabel(runner.problems[0]!);
  if (runner.state === "draining") return "Finishing running work";
  if (runner.state === "disabled") return "Disabled";
  if (runner.state === "paused") return "Paused";
  if (runner.health === "asleep") return "Asleep, wakes on new work";
  if (runner.health === "outside_hours") return "Outside hours";
  if (runner.connection_health === "offline" || ["offline", "stale", "revoked", "expired"].includes(runner.health)) return "Offline";
  if (runner.health !== "healthy" && runner.health !== "online") return "Needs attention";
  return undefined;
}
