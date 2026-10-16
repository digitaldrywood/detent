import React from "react";
import { Badge } from "../../components/ui/badge.tsx";
import type { ProviderCapacity } from "../../contracts/account.ts";
import { useNowMinute } from "../../hooks/useNowMinute.ts";
import { formatLocalTime, formatRelativeTime } from "./format.ts";

const usageMaxAge = 2 * 60 * 1000;

export function ProviderUsage({ capacity, now }: { readonly capacity: ProviderCapacity; readonly now?: number }): React.ReactElement | null {
  useNowMinute();
  const current = now ?? Date.now();
  if (capacity.billing === "api_key" || !capacity.usage_windows?.length) return null;
  return <ul className="space-y-1 text-xs text-muted-foreground" aria-label={`${capacity.backend} subscription usage`}>
    {capacity.usage_windows.map((window) => {
      const observed = window.observed_at ? Date.parse(window.observed_at) : Number.NaN;
      const age = current - observed;
      const stale = Number.isFinite(observed) && (age < 0 || age >= usageMaxAge || (window.reset_at !== undefined && current >= Date.parse(window.reset_at)));
      const unknown = !Number.isFinite(observed) || window.percent_used === undefined;
      const atRisk = capacity.billing === "subscription" && !stale && !unknown && window.percent_used !== undefined && window.percent_used >= 80;
      return <li key={window.kind} className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span>{window.kind.replaceAll("_", " ")} · {window.percent_used === undefined ? "Usage unknown" : `${window.percent_used}% used`}</span>
        {atRisk ? <Badge variant="warning" size="sm">At risk</Badge> : stale ? <Badge variant="secondary" size="sm">Stale</Badge> : unknown ? <Badge variant="secondary" size="sm">Unknown</Badge> : null}
        <span>{window.reset_at ? <>Resets <time dateTime={window.reset_at}>{formatLocalTime(window.reset_at)}</time></> : "Reset unknown"}</span>
        <span>{window.observed_at ? <>Observed <time dateTime={window.observed_at}>{formatRelativeTime(window.observed_at, current)}</time></> : "Observation unknown"}</span>
      </li>;
    })}
  </ul>;
}

export function billingLabel(capacity: ProviderCapacity): string {
  return capacity.billing === "subscription" ? "Subscription" : capacity.billing === "api_key" ? "API key" : "Billing unknown";
}
