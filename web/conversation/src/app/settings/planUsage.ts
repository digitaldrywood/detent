import type { ComparisonPlan, PlanReport } from "../../contracts/account.ts";
import { usageMeterState, type UsageUnit } from "../../components/ui/usage-meter.tsx";

const ALLOWANCES: Readonly<Record<string, { label: string; group: string; unit?: UsageUnit; description?: string; limitOnly?: boolean }>> = {
  projects: { label: "Projects", group: "Capacity" },
  unarchived_issues: { label: "Open issues", group: "Capacity", description: "Archived issues do not count." },
  collaboration_bytes: { label: "Workspace data", group: "Storage", unit: "bytes", description: "Issues, comments, conversations and attachment metadata." },
  history_records: { label: "History", group: "Storage", unit: "records", description: "Run and activity history after compaction." },
  artifact_retained_bytes: { label: "Artifact storage", group: "Storage", unit: "bytes", description: "Retained build outputs and uploads." },
  artifact_reserved_bytes: { label: "Reserved artifact storage", group: "Storage", unit: "bytes" },
  artifact_bytes: { label: "Artifact size", group: "Storage", unit: "bytes", limitOnly: true },
  artifact_upload_bytes: { label: "Upload size", group: "Storage", unit: "bytes", limitOnly: true },
  artifact_retention_seconds: { label: "Artifact retention", group: "Storage", unit: "seconds", limitOnly: true },
  relay_bytes: { label: "Relay data", group: "This hour", unit: "bytes" },
  api_mutations: { label: "API writes", group: "This hour" },
  ingested_events: { label: "Events received", group: "This hour" },
  members: { label: "Members", group: "Capacity" },
  repositories: { label: "Repositories", group: "Capacity" },
  registered_runners: { label: "Registered runners", group: "Capacity" },
  connected_runners: { label: "Connected runners", group: "Capacity" },
  concurrent_work: { label: "Concurrent work", group: "Capacity" },
};

export function planUsageRows(plan: PlanReport) {
  return Object.entries(plan.allowances).map(([name, limit]) => ({
    name,
    limit,
    used: plan.usage[name] ?? 0,
    ...(ALLOWANCES[name] ?? { label: name.replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase()), group: "Capacity" }),
  })).sort((a, b) => Object.keys(ALLOWANCES).indexOf(a.name) - Object.keys(ALLOWANCES).indexOf(b.name));
}

export function approachingPlanLimits(plan: PlanReport) {
  return planUsageRows(plan).filter((row) =>
    row.group !== "This hour" && !row.limitOnly &&
    (row.limit > 0 || row.used > 0) && usageMeterState(row.used, row.limit) !== "normal",
  );
}

export function nextFittingPlan(plan: PlanReport, comparisons: readonly ComparisonPlan[]): ComparisonPlan | undefined {
  const currentPrice = plan.monthly_usd_cents ?? 0;
  return comparisons.toSorted((a, b) => (a.monthly_usd_cents ?? Infinity) - (b.monthly_usd_cents ?? Infinity)).find((candidate) =>
    candidate.id !== plan.effective_base.id && (candidate.monthly_usd_cents ?? 0) > currentPrice &&
    planUsageRows(plan).filter((row) => row.group !== "This hour" && !row.limitOnly && row.used > 0).every((row) => {
      if (row.name.startsWith("artifact_") && !candidate.features.includes("hosted_artifacts")) return false;
      const limit = candidate.allowances[row.name];
      return limit === undefined || (limit > 0 && row.used / limit < 0.8);
    }),
  );
}
