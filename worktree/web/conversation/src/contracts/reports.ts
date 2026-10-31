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
  cancelled: Schema.Number,
  cost_usd: Schema.Number,
  cost_p50_usd: Schema.optional(Schema.Number),
  cost_p90_usd: Schema.optional(Schema.Number),
  system: Duration,
  lead: Duration,
  working: Duration,
  partial: Schema.Boolean,
  lanes: Schema.Array(Lane),
});
const AgingItem = Schema.Struct({
  work_item_id: Schema.String,
  lane: Schema.String,
  entered_at: Schema.String,
  hours: Schema.Number,
  lane_p90_hours: Schema.optional(Schema.Number),
  since_todo_hours: Schema.optional(Schema.Number),
});
const Page = <S extends Schema.Top>(item: S) =>
  Schema.Struct({
    items: Schema.Array(item),
    next_offset: Schema.optional(Schema.Number),
  });
const QualityCounts = Schema.Struct({
  worked_items: Schema.Number,
  reworked_items: Schema.Number,
  landed_versions: Schema.Number,
  escaped_versions: Schema.Number,
  escapes: Schema.Number,
  infrastructure: Schema.Number,
  infrastructure_returns: Schema.Number,
  pending_classification: Schema.Number,
  rework_percent: Schema.NullOr(Schema.Number),
  escape_percent: Schema.NullOr(Schema.Number),
  causes: Schema.Record(Schema.String, Schema.Number),
});
const Quality = Schema.Struct({
  ...QualityCounts.fields,
  source: Schema.String,
  coverage: Schema.String,
  partial: Schema.Boolean,
  unavailable: Schema.Array(Schema.String),
  buckets: Schema.Array(Schema.Struct({
    ...QualityCounts.fields,
    from: Schema.String,
    to: Schema.String,
  })),
});
const ClassTotal = Schema.Struct({
  class: Schema.String,
  kind: Schema.String,
  count: Schema.Number,
  issues: Schema.Number,
  cost_usd: Schema.Number,
  work_items: Schema.optional(Schema.Array(Schema.String)),
  first_seen: Schema.optional(Schema.String),
  last_seen: Schema.optional(Schema.String),
  tracking_work_item_id: Schema.optional(Schema.String),
});
const SeriesBucket = Schema.Struct({
  from: Schema.String,
  to: Schema.String,
  done: Schema.Number,
  prior_done: Schema.optional(Schema.Number),
  failed: Schema.Number,
  failed_infrastructure: Schema.Number,
  failed_issue: Schema.Number,
  cycle: Duration,
  lead: Duration,
  lane_seconds: Schema.Record(Schema.String, Schema.Number),
  stage_cost: Schema.Record(Schema.String, Schema.Number),
  not_shipped_cost: Schema.Number,
  rework_classes: Schema.Record(Schema.String, Schema.Number),
});
const Facet = Schema.Struct({ id: Schema.String, name: Schema.String });
/** The values the filter chips offer, read apart from the report. */
export const ReportsFacets = Schema.Struct({
  labels: Schema.Array(Schema.String),
  filed_by: Schema.Array(Facet),
  runners: Schema.Array(Facet),
  models: Schema.Array(Schema.String),
  stages: Schema.Array(Schema.String),
});
export const ReportsReport = Schema.Struct({
  prior_recorded: Schema.optional(Schema.Boolean),
  rework_classes: Schema.Array(ClassTotal),
  failure_classes: Schema.Array(ClassTotal),
  series: Schema.optional(Schema.Array(SeriesBucket)),
  cancelled: Schema.optional(Schema.Number),
  previous_cost_usd: Schema.optional(Schema.Number),
  previous_first_try_percent: Schema.optional(Schema.NullOr(Schema.Number)),
  previous_failed: Schema.optional(Schema.Number),
  failed_issues: Schema.optional(Schema.Number),
  open_work: Schema.optional(
    Schema.Struct({
      count: Schema.Number,
      held: Schema.Number,
      stuck: Schema.Number,
      previous_stuck: Schema.optional(Schema.Number),
      items: Schema.Array(AgingItem),
    }),
  ),
  prior: Schema.optional(
    Schema.Struct({
      rework_percent: Schema.NullOr(Schema.Number),
      infrastructure_returns: Schema.optional(Schema.Number),
      lane_seconds: Schema.Record(Schema.String, Schema.Number),
    }),
  ),
  runners: Schema.optional(
    Schema.Array(
      Schema.Struct({
        runner_id: Schema.String,
        name: Schema.String,
        sessions: Schema.Number,
        succeeded: Schema.Number,
        duration: Duration,
        cost_usd: Schema.Number,
      }),
    ),
  ),
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
      attempts: Schema.optional(Schema.Number),
      shipped: Schema.optional(Schema.Boolean),
      state: Schema.optional(Schema.String),
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
    quality: Schema.optional(Quality),
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
      aging: Page(AgingItem),
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
export type ReportsRange = "24h" | "48h" | "7d" | "30d" | "90d";
export type ReportsFacets = typeof ReportsFacets.Type;
/** A custom range: its first and last UTC days, YYYY-MM-DD. */
export interface ReportsCustomRange {
  readonly from: string;
  readonly to: string;
}
/** Filter values per chip; an empty list leaves that dimension unfiltered. */
export interface ReportsFilters {
  readonly labels: readonly string[];
  readonly priorities: readonly string[];
  readonly types: readonly string[];
  readonly filedBy: readonly string[];
  readonly runners: readonly string[];
  readonly models: readonly string[];
  readonly stages: readonly string[];
}
export type ReportsTimeView = "system" | "lead";

const ProductivityMetrics = Schema.Struct({
  filed: Schema.Number,
  completed: Schema.Number,
  cancelled: Schema.Number,
  net_backlog: Schema.Number,
  changes_landed: Schema.Number,
  worked: Schema.Number,
  reworked: Schema.Number,
  completion_cost_usd: Schema.Number,
  cost_per_completed_usd: Schema.NullOr(Schema.Number),
  cycle_p50_seconds: Schema.NullOr(Schema.Number),
  rework_percent: Schema.NullOr(Schema.Number),
});
const ProductivityCount = Schema.Struct({ filed: Schema.Number, completed: Schema.Number });
const ProductivityBucket = Schema.Struct({
  from: Schema.String,
  to: Schema.String,
  ...ProductivityCount.fields,
  projects: Schema.Record(Schema.String, ProductivityCount),
});
const ProductivityActor = Schema.Struct({
  kind: Schema.String,
  id: Schema.optional(Schema.String),
  name: Schema.optional(Schema.String),
  current: Schema.Number,
  previous: Schema.Number,
});
const ProductivityWindow = Schema.Struct({ from: Schema.String, to: Schema.String });
export const OrganizationReports = Schema.Struct({
  organization_id: Schema.String,
  prior_recorded: Schema.Boolean,
  window: ProductivityWindow,
  previous_window: ProductivityWindow,
  current: ProductivityMetrics,
  previous: ProductivityMetrics,
  projects: Schema.Array(Schema.Struct({ project_id: Schema.String, current: ProductivityMetrics, previous: ProductivityMetrics })),
  series: Schema.Array(ProductivityBucket),
  previous_series: Schema.Array(ProductivityBucket),
  filed_by: Schema.Array(ProductivityActor),
  completed_by: Schema.Array(ProductivityActor),
});
export type OrganizationReports = typeof OrganizationReports.Type;
