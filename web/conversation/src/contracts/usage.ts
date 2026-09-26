import * as Schema from "effect/Schema";

/** The four windows the range control offers (§17.5). */
export const UsageRange = Schema.Literals(["24h", "7d", "30d", "90d"]);
export type UsageRange = typeof UsageRange.Type;

/** True for a string the range control can ask the hub for. */
export function isUsageRange(value: unknown): value is UsageRange {
  return value === "24h" || value === "7d" || value === "30d" || value === "90d";
}

/** The window the report covers, as the hub resolved it. */
export const UsageWindow = Schema.Struct({
  from: Schema.String,
  to: Schema.String,
});
export type UsageWindow = typeof UsageWindow.Type;

/** The hero numbers: what the window cost, processed and ran. */
export const UsageTotal = Schema.Struct({
  cost: Schema.Number,
  tokens: Schema.Number,
  sessions: Schema.Number,
});
export type UsageTotal = typeof UsageTotal.Type;

/**
 * One provider row under the hero total. `share` is the provider's share of
 * the window's cost, as a fraction: the hub computes it so the rows always add
 * up to what the hero says, even where the client is showing a rounded number.
 */
export const UsageProvider = Schema.Struct({
  id: Schema.String,
  label: Schema.String,
  sessions: Schema.Number,
  cost: Schema.Number,
  share: Schema.Number,
  tokens: Schema.Number,
});
export type UsageProvider = typeof UsageProvider.Type;

/**
 * One period of the chart and of the by-day breakdown. `by_provider` is cost
 * per provider id, which is what the chart's layered series and the breakdown
 * table's per-provider columns both read.
 */
export const UsageDay = Schema.Struct({
  day: Schema.String,
  cost: Schema.Number,
  tokens: Schema.Number,
  by_provider: Schema.Record(Schema.String, Schema.Number),
});
export type UsageDay = typeof UsageDay.Type;

/** The five numbers of the Totals grid. */
export const UsageTotals = Schema.Struct({
  processed: Schema.Number,
  cached_input: Schema.Number,
  uncached_input: Schema.Number,
  output: Schema.Number,
  cache_savings: Schema.Number,
});
export type UsageTotals = typeof UsageTotals.Type;

/** One model row of the Breakdown table. */
export const UsageModel = Schema.Struct({
  model: Schema.String,
  provider: Schema.String,
  cost: Schema.Number,
  share: Schema.Number,
  tokens: Schema.Number,
});
export type UsageModel = typeof UsageModel.Type;

export const UsageBreakdown = Schema.Struct({
  by_model: Schema.Array(UsageModel),
  by_day: Schema.Array(UsageDay),
});
export type UsageBreakdown = typeof UsageBreakdown.Type;

/**
 * One allowance of the entitlement, as the Limits tab draws it. The shape is
 * `hubserver.HostedEntitlement`'s pair rather than a bare number, because the
 * tab is about how much of the allowance the window has spent.
 */
export const UsageAllowance = Schema.Struct({
  used: Schema.Number,
  limit: Schema.Number,
});
export type UsageAllowance = typeof UsageAllowance.Type;

/**
 * One runner's contribution to the window. `busy_seconds` is time under lease
 * and `capacity_used` the share of its concurrency the window consumed, so the
 * Runners tab can say what a host cost as well as what it did.
 */
export const UsageRunner = Schema.Struct({
  id: Schema.String,
  display_name: Schema.String,
  sessions: Schema.Number,
  tokens: Schema.Number,
  cost: Schema.Number,
  busy_seconds: Schema.Number,
  capacity_used: Schema.Number,
});
export type UsageRunner = typeof UsageRunner.Type;

/**
 * The whole report. `currency` is optional and defaults to USD on the client,
 * because §17.5 writes cost as a number and the hub's price table is priced in
 * one currency at a time.
 */
export const UsageReport = Schema.Struct({
  range: UsageWindow,
  total: UsageTotal,
  providers: Schema.Array(UsageProvider),
  daily: Schema.Array(UsageDay),
  totals: UsageTotals,
  breakdown: UsageBreakdown,
  limits: Schema.Record(Schema.String, UsageAllowance),
  runners: Schema.Array(UsageRunner),
  currency: Schema.optional(Schema.String),
});
export type UsageReport = typeof UsageReport.Type;

export const decodeUsageReport = Schema.decodeUnknownSync(UsageReport);
