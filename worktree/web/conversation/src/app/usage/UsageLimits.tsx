import React from "react";

import { cn } from "../../lib/utils.ts";
import type { AllowanceTotals } from "./adapter.ts";
import { formatCount } from "./usageFormat.ts";

/**
 * An allowance key as a label: `api_mutations` is "API mutations", everything
 * else swaps its underscores for spaces. The hub names these, not the client,
 * so an unknown key still reads as words rather than as an identifier.
 */
export function limitLabel(name: string): string {
  return name.replace(/^api_/, "API ").replaceAll("_", " ");
}

export function UsageLimitsSection({
  limits,
}: {
  readonly limits: readonly AllowanceTotals[];
}): React.ReactElement {
  if (limits.length === 0) {
    return (
      <section className="flex flex-col gap-2">
        <h2 className="text-sm font-medium text-foreground">Limits</h2>
        <p className="text-sm text-muted-foreground">
          This plan reports no allowances for the current window.
        </p>
      </section>
    );
  }

  return (
    <section className="flex flex-col gap-3">
      <h2 className="text-sm font-medium text-foreground">Limits</h2>
      <ul className="grid grid-cols-1 gap-x-8 gap-y-5 md:grid-cols-2">
        {limits.map((limit) => {
          const fraction = limit.limit === 0 ? 0 : Math.min(1, limit.used / limit.limit);
          return (
            <li key={limit.name} className="flex min-w-0 flex-col gap-1.5">
              <div className="flex items-baseline justify-between gap-4">
                <span className="truncate text-sm text-foreground capitalize">
                  {limitLabel(limit.name)}
                </span>
                <span
                  className={cn(
                    "shrink-0 text-sm font-medium tabular-nums",
                    limit.overLimit ? "text-warning-foreground" : "text-foreground",
                  )}
                >
                  {formatCount(limit.used)}
                  <span className="text-muted-foreground"> / {formatCount(limit.limit)}</span>
                </span>
              </div>
              <div
                role="progressbar"
                aria-label={`${limitLabel(limit.name)} used`}
                aria-valuenow={limit.used}
                aria-valuemin={0}
                aria-valuemax={limit.limit}
                className="h-1.5 w-full overflow-hidden rounded-full bg-input/60"
              >
                <div
                  className={cn(
                    "h-full rounded-full",
                    limit.overLimit ? "bg-warning" : "bg-foreground/60",
                  )}
                  style={{ width: `${Math.round(fraction * 100)}%` }}
                />
              </div>
              {limit.overLimit ? (
                <span className="text-xs text-warning-foreground">over limit</span>
              ) : null}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
