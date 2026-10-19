import React from "react";
import { RotateCwIcon } from "lucide-react";
import { Badge } from "../../components/ui/badge.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { usageMeterState } from "../../components/ui/usage-meter.tsx";
import type { ProviderCapacity } from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";
import { useNowMinute } from "../../hooks/useNowMinute.ts";
import { formatLocalTime, formatRelativeTime } from "./format.ts";

const usageMaxAge = 2 * 60 * 1000;

type UsageWindow = NonNullable<ProviderCapacity["usage_windows"]>[number];

function windowStatus(capacity: ProviderCapacity, window: UsageWindow, current: number): { stale: boolean; unknown: boolean; atRisk: boolean } {
  const observed = window.observed_at ? Date.parse(window.observed_at) : Number.NaN;
  const age = current - observed;
  const stale = Number.isFinite(observed) && (age < 0 || age >= usageMaxAge || (window.reset_at !== undefined && current >= Date.parse(window.reset_at)));
  const unknown = !Number.isFinite(observed) || window.percent_used === undefined;
  const atRisk = capacity.billing === "subscription" && !stale && !unknown && window.percent_used !== undefined && window.percent_used >= 80;
  return { stale, unknown, atRisk };
}

function countdown(iso: string, current: number): string {
  const remaining = Date.parse(iso) - current;
  if (!Number.isFinite(remaining) || remaining <= 0) return "now";
  const minutes = Math.ceil(remaining / 60_000);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.round(minutes / 60);
  return hours < 48 ? `${hours}h` : `${Math.round(hours / 24)}d`;
}

const WINDOW_ORDER = ["five_hour", "weekly"];
const WINDOW_LABELS: Record<string, string> = { five_hour: "5-hour", weekly: "Weekly" };

export function usageWindowKinds(capacities: readonly ProviderCapacity[]): string[] {
  const kinds = new Set(capacities.filter((capacity) => capacity.billing !== "api_key").flatMap((capacity) => capacity.usage_windows?.map((window) => window.kind) ?? []));
  const rank = (kind: string) => { const index = WINDOW_ORDER.indexOf(kind); return index === -1 ? WINDOW_ORDER.length : index; };
  return [...kinds].sort((left, right) => rank(left) - rank(right) || left.localeCompare(right));
}

export function usageWindowLabel(kind: string): string {
  const label = kind.replaceAll("_", " ");
  return WINDOW_LABELS[kind] ?? label.charAt(0).toUpperCase() + label.slice(1);
}

function Hint({ label, children, className }: { readonly label: string; readonly children: React.ReactNode; readonly className?: string }): React.ReactElement {
  return <Tooltip>
    <TooltipTrigger render={<span className={cn("inline-flex items-center gap-1", className)} tabIndex={0} aria-label={label} />}>{children}</TooltipTrigger>
    <TooltipPopup side="top">{label}</TooltipPopup>
  </Tooltip>;
}

export function UsageWindowCell({ capacity, kind, now }: { readonly capacity: ProviderCapacity; readonly kind: string; readonly now?: number }): React.ReactElement {
  useNowMinute();
  const current = now ?? Date.now();
  const window = capacity.usage_windows?.find((candidate) => candidate.kind === kind);
  if (!window) return <span className="text-muted-foreground/60">–</span>;
  const { stale, unknown, atRisk } = windowStatus(capacity, window, current);
  const percent = window.percent_used;
  const state = percent === undefined ? "normal" : usageMeterState(percent, 100);
  const dimmed = stale || unknown;
  const usage = `${kind.replaceAll("_", " ")} · ${percent === undefined ? "Usage unknown" : `${percent}% used`}${atRisk ? " · At risk" : stale ? " · Stale" : unknown ? " · Unknown" : ""}`;
  return <span className="inline-grid grid-cols-[4rem_2.75rem_2.75rem] items-center gap-x-2 text-muted-foreground" data-usage-window={window.kind}>
    <Hint label={usage} className="col-span-2 grid grid-cols-subgrid">
      <span role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent ?? 0} aria-label={usage} className={cn("h-1.5 w-16 overflow-hidden rounded-full bg-muted", dimmed && "outline-1 outline-dashed outline-border")}>
        <span className={cn("block h-full rounded-full", dimmed ? "bg-muted-foreground/40" : state === "reached" ? "bg-error" : state === "warning" ? "bg-warning" : "bg-primary")} style={{ width: `${Math.min(100, Math.max(0, percent ?? 0))}%` }} />
      </span>
      <span className={cn("text-right tabular-nums", !dimmed && (state === "reached" ? "text-error-foreground" : state === "warning" ? "text-warning-foreground" : "text-foreground"))}>{percent === undefined ? "?" : `${percent}%`}</span>
    </Hint>
    <Hint label={window.reset_at ? `Resets ${formatLocalTime(window.reset_at)}` : "Reset unknown"} className="justify-end tabular-nums">
      <RotateCwIcon aria-hidden="true" className="size-3" />
      {window.reset_at ? countdown(window.reset_at, current) : "?"}
    </Hint>
  </span>;
}

export function ProviderFreshness({ capacity, now }: { readonly capacity: ProviderCapacity; readonly now?: number }): React.ReactElement | null {
  useNowMinute();
  const current = now ?? Date.now();
  const windows = capacity.billing === "api_key" ? [] : capacity.usage_windows ?? [];
  if (windows.length === 0) return null;
  const observed = windows.map((window) => window.observed_at).filter((at): at is string => at !== undefined).sort()[0];
  const missing = windows.some((window) => !window.observed_at);
  const stale = windows.some((window) => windowStatus(capacity, window, current).stale);
  const label = missing ? "Observation unknown" : `${stale ? "Stale · observed" : "Observed"} ${formatRelativeTime(observed!, current)}`;
  return <Hint label={label}>
    <span data-freshness={missing ? "unknown" : stale ? "stale" : "fresh"} className={cn("size-2 rounded-full", missing ? "bg-muted-foreground/50" : stale ? "bg-warning" : "bg-success")} />
  </Hint>;
}

export function ProviderUsage({ capacity, now }: { readonly capacity: ProviderCapacity; readonly now?: number }): React.ReactElement | null {
  useNowMinute();
  const current = now ?? Date.now();
  if (capacity.billing === "api_key" || !capacity.usage_windows?.length) return null;
  return <ul className="space-y-1 text-xs text-muted-foreground" aria-label={`${capacity.backend} subscription usage`}>
    {capacity.usage_windows.map((window) => {
      const { stale, unknown, atRisk } = windowStatus(capacity, window, current);
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
