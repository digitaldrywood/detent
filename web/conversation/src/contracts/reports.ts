import * as Schema from "effect/Schema";

const Duration = Schema.Struct({
  count: Schema.Number,
  seconds: Schema.Number,
  p50_seconds: Schema.Number,
  p90_seconds: Schema.Number,
});
const Lane = Schema.Struct({
  lane: Schema.String,
  group: Schema.String,
  ...Duration.fields,
});
const Completion = Schema.Struct({
  done: Schema.Number,
  system: Duration,
  lead: Duration,
  working: Duration,
  partial: Schema.Boolean,
  lanes: Schema.Array(Lane),
});
const Page = <S extends Schema.Top>(item: S) =>
  Schema.Struct({
    items: Schema.Array(item),
    next_offset: Schema.optional(Schema.Number),
  });
export const ReportsReport = Schema.Struct({
  completion: Completion,
  previous: Completion,
  tokens: Schema.Number,
  input: Schema.Number,
  cached: Schema.Number,
  cost_usd: Schema.Number,
  first_try_percent: Schema.NullOr(Schema.Number),
  unavailable: Schema.Array(Schema.String),
  titles: Schema.Record(Schema.String, Schema.String),
  coverage: Schema.Array(
    Schema.Struct({
      source: Schema.String,
      observed: Schema.NullOr(Schema.Number),
      total: Schema.NullOr(Schema.Number),
    }),
  ),
  stages: Schema.Array(
    Schema.Struct({
      stage: Schema.String,
      model: Schema.String,
      effort: Schema.String,
      sessions: Schema.Number,
      succeeded: Schema.Number,
      duration: Duration,
      tokens: Schema.Number,
      input: Schema.Number,
      cached: Schema.Number,
      cost_usd: Schema.Number,
      issues: Schema.Number,
      usage_observed: Schema.Number,
      disabled: Schema.Boolean,
    }),
  ),
  spend: Schema.Array(
    Schema.Struct({
      work_item_id: Schema.String,
      title: Schema.String,
      cost_usd: Schema.Number,
    }),
  ),
  throughput: Schema.Array(
    Schema.Struct({
      from: Schema.String,
      to: Schema.String,
      done: Schema.Number,
      failed: Schema.Number,
      slots: Schema.NullOr(Schema.Number),
    }),
  ),
  analytics: Schema.Struct({
    project_id: Schema.String,
    window: Schema.Struct({ from: Schema.String, to: Schema.String }),
    partial: Schema.Boolean,
    unavailable: Schema.Array(Schema.String),
    lane_residence: Schema.Struct({
      source: Schema.String,
      coverage: Schema.String,
      partial: Schema.Boolean,
      issues_observed: Schema.Number,
      events_observed: Schema.Number,
      lanes: Schema.Array(Lane),
      system_total: Duration,
      held_total: Duration,
      lead_total: Duration,
      aging: Page(
        Schema.Struct({
          work_item_id: Schema.String,
          lane: Schema.String,
          entered_at: Schema.String,
          hours: Schema.Number,
          lane_p90_hours: Schema.optional(Schema.Number),
        }),
      ),
      rework_causes: Schema.Array(
        Schema.Struct({
          from_state: Schema.String,
          reason_detail: Schema.String,
          count: Schema.Number,
        }),
      ),
    }),
    failure_signatures: Page(
      Schema.Struct({
        signature: Schema.String,
        example: Schema.String,
        class: Schema.String,
        count: Schema.Number,
        work_items: Schema.Array(Schema.String),
        work_items_count: Schema.Number,
        work_items_partial: Schema.Boolean,
        first_seen: Schema.String,
        last_seen: Schema.String,
        cost_usd: Schema.Number,
      }),
    ),
  }),
});
export type ReportsReport = typeof ReportsReport.Type;
export type ReportsRange = "24h" | "48h" | "7d" | "30d";
export type ReportsTimeView = "system" | "lead";
