import * as Schema from "effect/Schema";

import { OrganizationId, ProjectId, NativeWorkItemId } from "./work.ts";

const ActivityAttempt = Schema.Struct({
  work_item_id: NativeWorkItemId,
  number: Schema.Number,
  title: Schema.String,
  project_id: ProjectId,
  project_name: Schema.String,
  runner_id: Schema.String,
  attempt_id: Schema.String,
  stage: Schema.optional(Schema.String),
  phase: Schema.optional(Schema.String),
  stage_started_at: Schema.optional(Schema.String),
  stage_elapsed_seconds: Schema.optional(Schema.Number),
  started_at: Schema.String,
  session_id: Schema.optional(Schema.String),
  workspace_ids: Schema.Array(Schema.String),
  change_id: Schema.optional(Schema.String),
  pull_request_url: Schema.optional(Schema.String),
  outcome: Schema.optional(Schema.Literals(["succeeded", "failed", "cancelled", "interrupted"])),
  finished_at: Schema.optional(Schema.String),
  stage_duration_seconds: Schema.optional(Schema.Number),
  partial: Schema.Boolean,
});

export const ActivityReport = Schema.Struct({
  organization_id: OrganizationId,
  observed_at: Schema.String,
  window: Schema.Struct({ from: Schema.String, to: Schema.String, bucket_ns: Schema.Number }),
  running: Schema.Array(ActivityAttempt),
  finished: Schema.Array(ActivityAttempt),
  typical_durations: Schema.Array(Schema.Struct({
    project_id: ProjectId,
    stage: Schema.String,
    count: Schema.Number,
    seconds: Schema.Number,
    p50_seconds: Schema.Number,
    p90_seconds: Schema.Number,
    partial: Schema.Boolean,
  })),
  population_limit: Schema.Number,
  partial: Schema.Boolean,
});
export type ActivityReport = typeof ActivityReport.Type;
export type ActivityFilters = {
  project_id?: string;
  runner_id?: string;
  limit?: number;
  from?: string;
  to?: string;
};
