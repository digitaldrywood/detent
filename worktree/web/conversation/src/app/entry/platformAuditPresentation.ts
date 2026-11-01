import { type PlatformAuditRow } from "./api.ts";

type AuditAction = {
  label: string;
  variant: "info" | "warning" | "error" | "secondary" | "success";
};

const actions: Record<string, AuditAction> = {
  ai_credits_granted: { label: "AI credits granted", variant: "info" },
  ai_credits_adjusted: { label: "AI credits adjusted", variant: "info" },
  plan_granted: { label: "Plan granted", variant: "info" },
  plan_revoked: { label: "Plan revoked", variant: "warning" },
  support_requested: { label: "Support access requested", variant: "warning" },
  support_started: { label: "Support access started", variant: "warning" },
  support_ended: { label: "Support access ended", variant: "secondary" },
  organization_requested: { label: "Tenant requested", variant: "info" },
  organization_ready: { label: "Tenant ready", variant: "success" },
  organization_failed: { label: "Tenant failed", variant: "error" },
  organization_deleted: { label: "Tenant deleted", variant: "secondary" },
  member_added: { label: "Staff member added", variant: "info" },
  member_removed: { label: "Staff member removed", variant: "warning" },
  member_role_changed: { label: "Staff role changed", variant: "info" },
};

function readable(value: string): string {
  const words = value.replaceAll(/[_:.\-]+/g, " ").trim();
  return words === ""
    ? "Other activity"
    : words[0]!.toUpperCase() + words.slice(1).replace(/\bai\b/gi, "AI");
}

export function auditAction(event: string): AuditAction {
  return actions[event] ?? { label: readable(event), variant: "secondary" };
}

export function auditActor(actor: string): string {
  if (actor.includes("@")) return actor;
  if (actor === "system" || actor === "entry") return "System";
  if (
    /^(platform[_:-])?(service[_:-]?token|service[_:-]?key)([_:-]|$)/i.test(
      actor,
    )
  )
    return "Platform service token";
  return "Unknown account";
}

export function auditWhen(at: string): string {
  const date = new Date(at);
  if (Number.isNaN(date.valueOf())) return "Unknown time";
  return `${new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", year: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: "UTC" }).format(date)} UTC`;
}

export function auditTenant(
  row: PlatformAuditRow,
  tenants: readonly { id: string; name: string }[],
): string {
  return (
    row.organization_name ||
    tenants.find((tenant) => tenant.id === row.organization_id)?.name ||
    (row.organization_deleted ? "Deleted tenant" : "Unknown tenant")
  );
}

export function auditDetails(row: PlatformAuditRow): {
  summary: string;
  reason: string;
} {
  const separator = row.detail.indexOf(" · ");
  let summary = separator < 0 ? row.detail : row.detail.slice(0, separator);
  let reason = separator < 0 ? "" : row.detail.slice(separator + 3);
  if (row.event.startsWith("support_")) {
    summary = auditAction(row.event).label;
    reason = row.detail;
  } else if (row.event === "plan_granted" || row.event === "plan_revoked") {
    summary = summary
      .replace(
        /^(\S+) v(\d+)/,
        (_, plan: string, version: string) =>
          `${readable(plan)} (version ${version})`,
      )
      .replace(
        /until (\d{4}-\d{2}-\d{2})T\S+/,
        (_, date: string) =>
          `until ${new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" }).format(new Date(`${date}T00:00:00Z`))}`,
      );
  } else if (row.source === "organization_events") {
    summary = summary.replace(/^generation /, "Provisioning attempt ");
  }
  return { summary: summary || auditAction(row.event).label, reason };
}

export function auditCSV(
  rows: readonly PlatformAuditRow[],
  tenants: readonly { id: string; name: string }[],
  includeHeader = true,
): string {
  const cell = (value: string) =>
    `"${(/^[=+\-@\t\r]/.test(value) ? "'" : "") + value.replaceAll('"', '""')}"`;
  return (
    [
      ...(includeHeader ? [["When (UTC)", "Action", "Tenant", "By", "Details", "Reason"]] : []),
      ...rows.map((row) => {
        const detail = auditDetails(row);
        const tenant =
          row.organization_id === ""
            ? ""
            : auditTenant(row, tenants) +
              (row.organization_deleted ? " (Deleted)" : "");
        return [
          row.at,
          auditAction(row.event).label,
          tenant,
          auditActor(row.actor),
          detail.summary,
          detail.reason,
        ];
      }),
    ]
      .map((row) => row.map(cell).join(",") + "\r\n")
      .join("")
  );
}
