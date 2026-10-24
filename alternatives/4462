import { useId, type ReactNode } from "react";

import { formatCount, formatTokens } from "~/app/usage/usageFormat";
import { cn } from "~/lib/utils";

export type UsageUnit = "count" | "records" | "bytes" | "seconds";

export function formatUsageValue(
  value: number,
  unit: UsageUnit = "count",
): string {
  if (unit === "records") return `${formatTokens(value)} records`;
  if (unit === "seconds") {
    if (value >= 86400) return `${formatCount(value / 86400)} days`;
    if (value >= 3600) return `${formatCount(value / 3600)} hours`;
    if (value >= 60) return `${formatCount(value / 60)} minutes`;
    return `${formatCount(value)} seconds`;
  }
  if (unit !== "bytes") return formatCount(value);
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index =
    value > 0
      ? Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1)
      : 0;
  return `${new Intl.NumberFormat("en-US", { maximumFractionDigits: 2 }).format(value / 1024 ** index)} ${units[index]}`;
}

export function usageMeterState(
  used: number,
  limit: number,
): "normal" | "warning" | "reached" {
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
  direction = "maximum",
  valueText,
  statusText,
  layout = "stacked",
  detail,
  limitTone = "error",
}: {
  readonly label: string;
  readonly used: number;
  readonly limit: number;
  readonly unit?: UsageUnit;
  readonly description?: ReactNode;
  readonly notIncludedOn?: string;
  readonly limitOnly?: boolean;
  readonly unlimited?: boolean;
  readonly direction?: "maximum" | "minimum";
  readonly valueText?: string;
  readonly statusText?: string;
  readonly layout?: "stacked" | "row";
  readonly detail?: ReactNode;
  readonly limitTone?: "error" | "warning";
}) {
  const id = useId();
  const state =
    direction === "minimum"
      ? used <= limit
        ? "reached"
        : used <= limit * 1.2
          ? "warning"
          : "normal"
      : usageMeterState(used, limit);
  const excluded = notIncludedOn !== undefined;
  const text =
    valueText ??
    (excluded
      ? `Not included on ${notIncludedOn}`
      : unlimited
        ? `${formatUsageValue(used, unit)} used · no plan limit`
        : limitOnly
          ? `Up to ${formatUsageValue(limit, unit)}`
          : `${formatUsageValue(used, unit)} of ${formatUsageValue(limit, unit)}`);
  const status =
    statusText ??
    (state === "reached"
      ? "Limit reached"
      : state === "warning"
        ? "Approaching limit"
        : "Within limit");
  const maximum =
    direction === "minimum" ? Math.max(used, limit * 2, 1) : Math.max(limit, 1);

  const bar = (
    <div
      role="meter"
      aria-labelledby={`${id}-label`}
      aria-describedby={`${id}-detail`}
      aria-valuemin={0}
      aria-valuemax={unlimited ? Math.max(used, 1) : maximum}
      aria-valuenow={
        unlimited ? Math.max(used, 0) : Math.min(Math.max(used, 0), maximum)
      }
      aria-valuetext={text}
      className={cn(
        "h-1.5 overflow-hidden rounded-full bg-muted",
        layout === "row" &&
          "col-span-2 row-start-2 sm:col-span-1 sm:col-start-2 sm:row-start-1",
      )}
    >
      <div
        className={cn(
          "h-full rounded-full",
          state === "reached"
            ? limitTone === "warning" ? "bg-warning" : "bg-error"
            : state === "warning"
              ? "bg-warning"
              : "bg-foreground/60",
        )}
        style={{
          width: `${excluded || limitOnly || unlimited ? 0 : limit <= 0 && direction === "maximum" ? 100 : Math.min(100, Math.max(0, (direction === "minimum" ? used / maximum : used / limit) * 100))}%`,
        }}
      />
    </div>
  );
  if (layout === "row") {
    return (
      <div
        data-slot="usage-meter"
        className="grid grid-cols-[minmax(0,1fr)_auto_1.5rem] items-center gap-x-3 gap-y-2 py-2 text-sm sm:grid-cols-[184px_minmax(0,1fr)_180px_1.5rem] sm:gap-x-4"
      >
        <span id={`${id}-label`}>{label}</span>
        {bar}
        <span
          className={cn(
            "text-right tabular-nums sm:col-start-3",
            !limitOnly &&
              !unlimited &&
              (state === "reached"
                ? limitTone === "warning" ? "text-foreground" : "text-error-foreground"
                : state === "warning"
                  ? "text-warning-foreground"
                  : "text-foreground"),
          )}
        >
          {limitOnly ? (
            `Up to ${formatUsageValue(limit, unit)}`
          ) : (
            <>
              {formatUsageValue(used, unit)}{" "}
              <span className="text-muted-foreground">
                / {unlimited ? "Unlimited" : formatUsageValue(limit, unit)}
              </span>
            </>
          )}
        </span>
        <div className="col-start-3 row-start-1 sm:col-start-4">{detail}</div>
        <span id={`${id}-detail`} className="sr-only">
          {!limitOnly && !unlimited ? status : null} {description}
        </span>
      </div>
    );
  }

  return (
    <div className="min-w-0 space-y-1.5 text-sm" data-slot="usage-meter">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <span id={`${id}-label`} className="text-muted-foreground">
          {label}
        </span>
        <span
          className={cn(
            "font-medium tabular-nums",
            !excluded &&
              !limitOnly &&
              !unlimited &&
              (state === "reached"
                ? limitTone === "warning" ? "text-foreground" : "text-error-foreground"
                : state === "warning"
                  ? "text-warning-foreground"
                  : "text-foreground"),
          )}
        >
          {text}
        </span>
      </div>
      {bar}
      <div
        id={`${id}-detail`}
        className={cn(
          "space-y-1 text-xs text-muted-foreground",
          !excluded &&
            !limitOnly &&
            !unlimited &&
            (state === "reached"
              ? limitTone === "warning" ? undefined : "text-error-foreground"
              : state === "warning"
                ? "text-warning-foreground"
                : undefined),
        )}
      >
        {!excluded && !limitOnly && !unlimited && status !== "" ? (
          <p>
            {status}
            {statusText === undefined
              ? ` · ${formatUsageValue(Math.max(0, direction === "minimum" ? used - limit : limit - used), unit)} ${direction === "minimum" ? "above floor" : "left"}`
              : ""}
          </p>
        ) : null}
        {description ? <div>{description}</div> : null}
      </div>
    </div>
  );
}
