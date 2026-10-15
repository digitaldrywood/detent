import { formatChatTimestampTooltip } from "../../timestampFormat.ts";
import type { OrganizationEntitlements, PlanReference } from "./api.ts";

export function readableKey(value: string): string {
  return value
    .replaceAll("_", " ")
    .replaceAll("-", " ")
    .replace(/\bapi\b/gi, "API")
    .replace(/^./, (letter) => letter.toUpperCase());
}

export function planDisplayName(
  plan: PlanReference,
  value?: OrganizationEntitlements,
): string {
  const configured = value?.plans.find(
    (item) => item.id === plan.id && item.version === plan.version,
  )?.name;
  if (configured) return configured;
  if (plan.id === "pilot_free") return "Pilot (legacy)";
  if (plan.id === "comp_team") return "Team";
  return readableKey(plan.id);
}

export function platformTime(value: string | null): string {
  return value === null
    ? "Never"
    : formatChatTimestampTooltip(value, "locale") || "Unavailable";
}

export const SUPPORT_REASON_LABELS: Record<string, string> = {
  "customer-request": "Customer asked for help",
  "account-recovery": "Account recovery",
  troubleshooting: "Troubleshooting",
};

export function auditDetail(
  value: string,
  entitlements?: OrganizationEntitlements,
): string {
  let result = value.replace(/generation \d+/g, "");
  result = result.replace(
    /([a-z][a-z0-9_]+) v(\d+)/g,
    (_, id: string, version: string) =>
      planDisplayName({ id, version: Number(version) }, entitlements),
  );
  result = result.replace(
    /\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+-]\d{2}:\d{2})/g,
    platformTime,
  );
  for (const [key, label] of Object.entries(SUPPORT_REASON_LABELS))
    result = result.replaceAll(key, label);
  return result;
}

export function tenantPlanGrant(value: OrganizationEntitlements) {
  return value.grants
    .filter(
      (grant) =>
        !(grant.scope?.length === 1 && grant.scope[0] === "model_choice"),
    )
    .toSorted((a, b) => Date.parse(b.starts_at) - Date.parse(a.starts_at))[0];
}
