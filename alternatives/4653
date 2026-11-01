import React from "react";
import type { FleetRunner, ProviderCapacity } from "../../contracts/account.ts";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { formatLocalTime } from "../fleet/format.ts";

export interface ProviderRow {
  readonly provider: string;
  readonly accounts: number;
  readonly used: number;
  readonly max: number;
  readonly availability: readonly string[];
  readonly status: readonly string[];
  readonly models: readonly string[];
}

export function summarizeProviders(runners: readonly FleetRunner[]): readonly ProviderRow[] {
  const rows = new Map<
    string,
    {
      provider: string;
      accounts: Map<string, { used: number; max: number }>;
      availability: Set<string>;
      status: Set<string>;
      models: Set<string>;
    }
  >();
  const capacities: ProviderCapacity[] = runners.flatMap((runner) => [...runner.provider_capacity]);
  const sharedProviders = new Set(capacities.filter((capacity) => !capacity.shared_account_alias).map((capacity) => capacity.provider));
  for (const capacity of capacities) {
    let row = rows.get(capacity.provider);
    if (row === undefined) {
      row = {
        provider: capacity.provider,
        accounts: new Map(),
        availability: new Set(),
        status: new Set(),
        models: new Set(),
      };
      rows.set(capacity.provider, row);
    }
    const account = sharedProviders.has(capacity.provider) ? "" : capacity.shared_account_alias ?? "";
    const pool = row.accounts.get(account);
    const max = capacity.max_concurrent ?? 0;
    row.accounts.set(account, {
      used: Math.max(pool?.used ?? 0, capacity.used),
      max: pool?.max && max ? Math.min(pool.max, max) : pool?.max || max,
    });
    row.availability.add(capacity.availability);
    row.status.add(capacity.reset_at ? "Rate limited until " + formatLocalTime(capacity.reset_at)
      : [capacity.availability, capacity.state].filter(Boolean).map((value) => value.replaceAll("_", " ")).join(" · "));
    for (const model of capacity.models ?? []) row.models.add(model);
  }
  return [...rows.values()].map((row) => ({
    provider: row.provider,
    accounts: row.accounts.size,
    used: [...row.accounts.values()].reduce((sum, pool) => sum + pool.used, 0),
    max: [...row.accounts.values()].reduce((sum, pool) => sum + pool.max, 0),
    availability: [...row.availability],
    status: [...row.status],
    models: [...row.models],
  }));
}

export function ProviderSummary({ runners }: { readonly runners: readonly FleetRunner[] }): React.ReactElement {
  const rows = summarizeProviders(runners);
  return (
    <div className="min-w-0 overflow-x-auto rounded-xl" tabIndex={0} role="region" aria-label="Provider accounts">
      <table className="w-full min-w-[640px] text-left text-xs">
        <thead className="border-b border-border/60 text-muted-foreground">
          <tr>{["Provider", "Accounts", "In use", "Models", "Status"].map((heading) => <th key={heading} scope="col" className="px-4 py-3 font-medium">{heading}</th>)}</tr>
        </thead>
        <tbody className="divide-y divide-border/50">
          {rows.map((row) => {
            const { provider, used, max, status } = row;
            return (
              <tr key={provider}>
                <th scope="row" className="px-4 py-4 font-medium">{provider}</th>
                <td className="px-4 py-4 tabular-nums">{row.accounts}</td>
                <td className="min-w-28 px-4 py-4">
                  <span className="tabular-nums">{used}/{max}</span>
                  <div role="progressbar" aria-label={provider + " in use"} aria-valuenow={used} aria-valuemin={0} aria-valuemax={max} className="mt-2 h-1 w-16 overflow-hidden rounded-full bg-accent">
                    <div className="h-full bg-primary" style={{ width: (max > 0 ? Math.min(100, used / max * 100) : 0) + "%" }} />
                  </div>
                </td>
                <td className="max-w-56 px-4 py-4 text-muted-foreground"><Tooltip><TooltipTrigger render={<span tabIndex={0} />} aria-label={`${row.models.length} models`}>{row.models.length} models</TooltipTrigger><TooltipPopup>{row.models.join(", ") || "No models reported"}</TooltipPopup></Tooltip></td>
                <td className="px-4 py-4">{status.map((value) => <p key={value} className={value.startsWith("Rate limited") ? "text-warning-foreground" : "text-muted-foreground"}>{value}</p>)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
