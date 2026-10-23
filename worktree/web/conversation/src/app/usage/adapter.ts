import React from "react";

import {
  decodeUsageReport,
  isUsageRange,
  type UsageRange,
  type UsageReport,
} from "../../contracts/usage.ts";
import { AccountError } from "../account/api.ts";

export type { UsageRange };
export { isUsageRange };

export interface ProviderTotals {
  readonly provider: string;
  readonly label: string;
  readonly sessions: number;
  readonly costUsd: number;
  readonly totalTokens: number;
  readonly costShare: number;
  readonly tokenShare: number;
}

export interface PeriodProviderTotals {
  readonly costUsd: number;
  readonly totalTokens: number;
}

export interface DailyTotals {
  readonly day: string;
  readonly costUsd: number;
  readonly totalTokens: number;
  readonly byProvider: ReadonlyMap<string, PeriodProviderTotals>;
}

export interface HourlyTotals {
  readonly hourStart: string;
  readonly costUsd: number;
  readonly totalTokens: number;
  readonly byProvider: ReadonlyMap<string, PeriodProviderTotals>;
}

export interface ModelTotals {
  readonly provider: string;
  readonly model: string;
  readonly costUsd: number;
  readonly totalTokens: number;
  readonly costShare: number;
}

/** One runner's contribution, for the Runners tab (§17.5's `runners`). */
export interface RunnerTotals {
  readonly id: string;
  readonly displayName: string;
  readonly sessions: number;
  readonly totalTokens: number;
  readonly costUsd: number;
  readonly busySeconds: number;
  readonly capacityUsed: number;
}

/** One entitlement allowance, for the Limits tab. */
export interface AllowanceTotals {
  readonly name: string;
  readonly used: number;
  readonly limit: number;
  readonly overLimit: boolean;
}

export interface MergedUsage {
  readonly costUsd: number;
  readonly totalTokens: number;
  readonly sessions: number;
  readonly cachedInputTokens: number;
  readonly uncachedInputTokens: number;
  readonly outputTokens: number;
  readonly costQuality: { readonly cacheSavingsUsd: number };
  readonly providers: readonly ProviderTotals[];
  readonly daily: readonly DailyTotals[];
  readonly hourly: readonly HourlyTotals[];
  readonly models: readonly ModelTotals[];
  readonly runners: readonly RunnerTotals[];
  readonly limits: readonly AllowanceTotals[];
  /** The window the hub resolved, which is what the header labels. */
  readonly from: string;
  readonly to: string;
  readonly currency: string;
}

/** The value a window with nothing in it draws, rather than a blank page. */
export const EMPTY_USAGE: MergedUsage = {
  costUsd: 0,
  totalTokens: 0,
  sessions: 0,
  cachedInputTokens: 0,
  uncachedInputTokens: 0,
  outputTokens: 0,
  costQuality: { cacheSavingsUsd: 0 },
  providers: [],
  daily: [],
  hourly: [],
  models: [],
  runners: [],
  limits: [],
  from: "",
  to: "",
  currency: "USD",
};

// --- Reshaping --------------------------------------------------------------

function isHourly(day: string): boolean {
  return day.includes("T");
}

function periodProviders(
  byProvider: Readonly<Record<string, number>>,
  tokens: number,
  cost: number,
): ReadonlyMap<string, PeriodProviderTotals> {
  const entries = new Map<string, PeriodProviderTotals>();
  for (const [provider, providerCost] of Object.entries(byProvider)) {
    // The report carries cost per provider per period, not tokens. Splitting
    // the period's tokens by the period's own cost share is the only division
    // that keeps the chart's token series adding up to the totals grid.
    const share = cost === 0 ? 0 : providerCost / cost;
    entries.set(provider, { costUsd: providerCost, totalTokens: tokens * share });
  }
  return entries;
}

export function toMergedUsage(report: UsageReport): MergedUsage {
  const hourly = report.daily.filter((entry) => isHourly(entry.day));
  const daily = report.daily.filter((entry) => !isHourly(entry.day));
  const tokens = report.total.tokens;
  const cost = report.total.cost;

  return {
    costUsd: cost,
    totalTokens: tokens,
    sessions: report.total.sessions,
    cachedInputTokens: report.totals.cached_input,
    uncachedInputTokens: report.totals.uncached_input,
    outputTokens: report.totals.output,
    costQuality: { cacheSavingsUsd: report.totals.cache_savings },
    providers: report.providers.map((provider) => ({
      provider: provider.id,
      label: provider.label,
      sessions: provider.sessions,
      costUsd: provider.cost,
      totalTokens: provider.tokens,
      costShare: provider.share,
      tokenShare: tokens === 0 ? 0 : provider.tokens / tokens,
    })),
    daily: daily.map((entry) => ({
      day: entry.day,
      costUsd: entry.cost,
      totalTokens: entry.tokens,
      byProvider: periodProviders(entry.by_provider, entry.tokens, entry.cost),
    })),
    hourly: hourly.map((entry) => ({
      hourStart: entry.day,
      costUsd: entry.cost,
      totalTokens: entry.tokens,
      byProvider: periodProviders(entry.by_provider, entry.tokens, entry.cost),
    })),
    models: report.breakdown.by_model.map((model) => ({
      provider: model.provider,
      model: model.model,
      costUsd: model.cost,
      totalTokens: model.tokens,
      costShare: model.share,
    })),
    runners: report.runners.map((runner) => ({
      id: runner.id,
      displayName: runner.display_name,
      sessions: runner.sessions,
      totalTokens: runner.tokens,
      costUsd: runner.cost,
      busySeconds: runner.busy_seconds,
      capacityUsed: runner.capacity_used,
    })),
    limits: Object.entries(report.limits)
      .map(([name, allowance]) => ({
        name,
        used: allowance.used,
        limit: allowance.limit,
        overLimit: allowance.used > allowance.limit,
      }))
      .toSorted((left, right) => left.name.localeCompare(right.name)),
    from: report.range.from,
    to: report.range.to,
    currency: report.currency ?? "USD",
  };
}

// --- The read ---------------------------------------------------------------

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface UsageApiOptions {
  /** Origin the API is served from. Empty string for same-origin. */
  readonly origin: string;
  /** `api_base` from the bootstrap payload, e.g. `/api/v2/organizations/org_1`. */
  readonly apiBase: string;
  readonly fetch?: FetchLike;
}

/** The query §17.5 defines, including the optional project narrowing. */
export function usagePath(
  apiBase: string,
  range: UsageRange,
  project?: string | null,
): string {
  const query = new URLSearchParams({ range });
  if (project !== undefined && project !== null && project !== "") query.set("project", project);
  return `${apiBase}/usage?${query.toString()}`;
}

/**
 * Reads one window. Failures arrive as the same typed `AccountError` the other
 * account screens render, so the page has one refusal state rather than two.
 */
export async function fetchUsage(
  options: UsageApiOptions,
  range: UsageRange,
  project?: string | null,
): Promise<MergedUsage> {
  const doFetch: FetchLike = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
  let response: Response;
  try {
    response = await doFetch(`${options.origin}${usagePath(options.apiBase, range, project)}`, {
      method: "GET",
      credentials: "same-origin",
      headers: { Accept: "application/json" },
    });
  } catch (cause) {
    throw new AccountError({
      status: 0,
      code: "network",
      message: cause instanceof Error ? cause.message : String(cause),
    });
  }
  const text = await response.text();
  let parsed: unknown = undefined;
  if (text.length > 0) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = undefined;
    }
  }
  if (!response.ok) {
    const body = parsed as { code?: string; message?: string } | undefined;
    throw new AccountError({
      status: response.status,
      code: body?.code ?? (response.status === 403 ? "forbidden" : "invalid"),
      message:
        body?.message !== undefined && body.message.length > 0
          ? body.message
          : response.status === 403
            ? "You do not have permission to read usage."
            : response.status === 404
              ? "This hub does not report usage yet."
              : `The request failed (${response.status}).`,
    });
  }
  try {
    return toMergedUsage(decodeUsageReport(parsed ?? {}));
  } catch (cause) {
    throw new AccountError({
      status: response.status,
      code: "invalid",
      message: `The hub returned an unexpected usage payload: ${
        cause instanceof Error ? cause.message : String(cause)
      }`,
    });
  }
}

export function useUsage(
  options: UsageApiOptions,
  range: UsageRange,
  project?: string | null,
): {
  readonly merged: MergedUsage;
  readonly isPending: boolean;
  readonly error: AccountError | null;
  readonly refresh: () => Promise<void>;
} {
  const [merged, setMerged] = React.useState<MergedUsage>(EMPTY_USAGE);
  const [isPending, setPending] = React.useState(true);
  const [error, setError] = React.useState<AccountError | null>(null);
  const generation = React.useRef(0);
  const origin = options.origin;
  const apiBase = options.apiBase;
  const doFetch = options.fetch;

  const run = React.useCallback(async () => {
    const mine = generation.current + 1;
    generation.current = mine;
    setPending(true);
    try {
      const next = await fetchUsage({ origin, apiBase, fetch: doFetch }, range, project);
      if (generation.current !== mine) return;
      setMerged(next);
      setError(null);
    } catch (cause) {
      if (generation.current !== mine) return;
      setMerged(EMPTY_USAGE);
      setError(
        cause instanceof AccountError
          ? cause
          : new AccountError({
              status: 0,
              code: "invalid",
              message: cause instanceof Error ? cause.message : String(cause),
            }),
      );
    } finally {
      if (generation.current === mine) setPending(false);
    }
  }, [apiBase, doFetch, origin, project, range]);

  React.useEffect(() => {
    void run();
    return () => {
      generation.current += 1;
    };
  }, [run]);

  return { merged, isPending, error, refresh: run };
}
