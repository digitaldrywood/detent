import { useId, type ReactNode } from "react";

import { formatCount, formatTokens } from "~/app/usage/usageFormat";
import { cn } from "~/lib/utils";

export type UsageUnit = "count" | "records" | "bytes" | "seconds";

export function formatUsageValue(value: number, unit: UsageUnit = "count"): string {
  if (unit === "records") return `${formatTokens(value)} records`;
  if (unit === "seconds") {
    if (value >= 86400) return `${formatCount(value / 86400)} days`;
    if (value >= 3600) return `${formatCount(value / 3600)} hours`;
    if (value >= 60) return `${formatCount(value / 60)} minutes`;
    return `${formatCount(value)} seconds`;
  }
  if (unit !== "bytes") return formatCount(value);
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = value > 0 ? Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1) : 0;
  return `${new Intl.NumberFormat("en-US", { maximumFractionDigits: 2 }).format(value / 1024 ** index)} ${units[index]}`;
}

export function usageMeterState(used: number, limit: number): "normal" | "warning" | "reached" {
  if (limit <= 0 || used >= limit) return "reached";
  return used / limit >= 0.8 ? "warning" : "normal";
}

export function UsageMeter({
  label,
  used,
  limit,
  unit = "count",
  description,
  notIncludedOn,
  limitOnly = false,
  unlimited = false,
}: {
  readonly label: string;
  readonly used: number;
  readonly limit: number;
  readonly unit?: UsageUnit;
  readonly description?: ReactNode;
  readonly notIncludedOn?: string;
  readonly limitOnly?: boolean;
  readonly unlimited?: boolean;
}) {
  const id = useId();
  const state = usageMeterState(used, limit);
  const excluded = notIncludedOn !== undefined;
  const text = excluded
    ? `Not included on ${notIncludedOn}`
    : unlimited
      ? `${formatUsageValue(used, unit)} used · no plan limit`
    : limitOnly
      ? `Up to ${formatUsageValue(limit, unit)}`
      : `${formatUsageValue(used, unit)} of ${formatUsageValue(limit, unit)}`;
  const status = state === "reached" ? "Limit reached" : state === "warning" ? "Approaching limit" : "Within limit";

  return (
    <div className="min-w-0 space-y-1.5 text-sm" data-slot="usage-meter">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <span id={`${id}-label`} className="text-muted-foreground">{label}</span>
        <span className={cn("font-medium tabular-nums", !excluded && !limitOnly && !unlimited && (state === "reached" ? "text-error-foreground" : state === "warning" ? "text-warning-foreground" : "text-foreground"))}>{text}</span>
      </div>
      <div
        role="meter"
        aria-labelledby={`${id}-label`}
        aria-describedby={`${id}-detail`}
        aria-valuemin={0}
        aria-valuemax={unlimited ? Math.max(used, 1) : Math.max(limit, 1)}
        aria-valuenow={unlimited ? Math.max(used, 0) : Math.min(Math.max(used, 0), Math.max(limit, 1))}
        aria-valuetext={text}
        className="h-1.5 overflow-hidden rounded-full bg-muted"
      >
        <div className={cn("h-full rounded-full", state === "reached" ? "bg-error" : state === "warning" ? "bg-warning" : "bg-foreground/60")} style={{ width: `${excluded || limitOnly || unlimited ? 0 : limit <= 0 ? 100 : Math.min(100, Math.max(0, used / limit * 100))}%` }} />
      </div>
      <div id={`${id}-detail`} className={cn("space-y-1 text-xs text-muted-foreground", !excluded && !limitOnly && !unlimited && (state === "reached" ? "text-error-foreground" : state === "warning" ? "text-warning-foreground" : undefined))}>
        {!excluded && !limitOnly && !unlimited ? <p>{status} · {formatUsageValue(Math.max(0, limit - used), unit)} left</p> : null}
        {description ? <div>{description}</div> : null}
      </div>
    </div>
  );
}
