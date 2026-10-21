import { formatChatTimestampTooltip } from "../../timestampFormat.ts";
import type {
  EntitlementPlan,
  OrganizationEntitlements,
  PlanReference,
} from "./api.ts";

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

const PLAN_TIERS = ["free", "starter", "team", "growth", "scale"];
const LEGACY_PLAN_IDS = new Set(["pilot_free", "comp_team"]);
const FEATURE_LABELS: Record<string, string> = {
  collaboration: "Collaboration",
  native_execution: "Runner work",
  github_integration: "GitHub",
  hosted_artifacts: "Artifact storage",
  model_choice: "Model choice",
};

export function featureLabel(feature: string): string {
  return FEATURE_LABELS[feature] ?? readableKey(feature);
}

export function planPrice(plan: EntitlementPlan): string {
  const cents = plan.monthly_usd_cents;
  if (cents === undefined || cents === null) return "Contact us";
  return `$${(cents / 100).toLocaleString("en-US")}/mo`;
}

export interface GrantChoice {
  readonly plan: EntitlementPlan;
  readonly group: "plans" | "other";
  readonly current: boolean;
}

/**
 * The plans staff can grant: the latest version of each catalog plan in tier
 * order, plus the organization's current grant when it is a legacy or older
 * version. The base plan is never offered because granting it changes nothing.
 */
export function grantChoices(value: OrganizationEntitlements): GrantChoice[] {
  const key = (plan: PlanReference) => `${plan.id}@${plan.version}`;
  const latest = new Map<string, number>();
  for (const plan of value.plans)
    latest.set(plan.id, Math.max(latest.get(plan.id) ?? 0, plan.version));
  const current = new Set(
    value.grants
      .filter(
        (grant) =>
          !(grant.scope?.length === 1 && grant.scope[0] === "model_choice"),
      )
      .map((grant) => key(grant.plan)),
  );
  const rank = (id: string) => {
    const index = PLAN_TIERS.indexOf(id);
    return index === -1 ? PLAN_TIERS.length : index;
  };
  return value.plans
    .filter((plan) => key(plan) !== key(value.base))
    .map((plan) => {
      const latestPlan =
        latest.get(plan.id) === plan.version && !LEGACY_PLAN_IDS.has(plan.id);
      return {
        plan,
        latestPlan,
        group: latestPlan && rank(plan.id) < PLAN_TIERS.length ? "plans" : "other",
        current: current.has(key(plan)),
      } as const;
    })
    .filter((choice) => choice.latestPlan || choice.current)
    .toSorted(
      (a, b) =>
        Number(a.group === "other") - Number(b.group === "other") ||
        rank(a.plan.id) - rank(b.plan.id) ||
        Number(b.current) - Number(a.current) ||
        planDisplayName(a.plan, value).localeCompare(
          planDisplayName(b.plan, value),
        ) ||
        b.plan.version - a.plan.version,
    )
    .map(({ plan, group, current }) => ({ plan, group, current }));
}
