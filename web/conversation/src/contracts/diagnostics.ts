import * as Schema from "effect/Schema";

const IdentityValue = Schema.Struct({ value: Schema.optional(Schema.String) });

export const AttemptDiagnostics = {
  runtime_freshness: Schema.optional(Schema.String),
  terminal_failure: Schema.optional(Schema.Struct({ summary: Schema.String })),
  disposition: Schema.optional(Schema.Struct({
    status: Schema.String,
    human_action: Schema.Boolean,
    final_summary: Schema.optional(Schema.String),
  })),
  usage: Schema.optional(Schema.Array(Schema.Struct({
    input: Schema.Number,
    output: Schema.Number,
    cost_estimate: Schema.Number,
    currency: Schema.String,
    reported_cost_micros: Schema.optional(Schema.Number),
    cost_coverage: Schema.optional(Schema.String),
  }))),
  runtime: Schema.optional(Schema.Struct({
    phase: Schema.String,
    identity: Schema.optional(Schema.Struct({
      resolved_model: Schema.optional(IdentityValue),
      requested_model: Schema.optional(IdentityValue),
      reasoning_effort: Schema.optional(IdentityValue),
    })),
    activity: Schema.optional(Schema.Struct({
      stage: Schema.String,
      coverage: Schema.String,
      instructions: Schema.NullOr(Schema.Array(Schema.Struct({ name: Schema.String, sha256: Schema.String }))),
      timing_summary: Schema.optional(Schema.Struct({
        breakdown: Schema.Struct({
          elapsed_seconds: Schema.Number,
          observed_seconds: Schema.Number,
          unknown_seconds: Schema.Number,
          concurrent_seconds: Schema.Number,
          by_kind_seconds: Schema.NullOr(Schema.Record(Schema.String, Schema.Number)),
        }),
      })),
    })),
  })),
};

const Decision = Schema.Struct({
  state: Schema.String,
  reason: Schema.optional(Schema.String),
  at: Schema.String,
});

export const IssueExplanation = Schema.Struct({
  observed_at: Schema.String,
  current_lane: Schema.Struct({ name: Schema.optional(Schema.String), freshness: Schema.String }),
  latest_transition: Schema.optional(Schema.Struct({ at: Schema.String, reason: Schema.optional(Schema.String) })),
  eligibility: Schema.Struct({
    state: Schema.String,
    source_state: Schema.String,
    current: Schema.optional(Decision),
    latest: Schema.optional(Decision),
  }),
  required_gate: Schema.Struct({
    state: Schema.String,
    source_state: Schema.String,
    reason: Schema.optional(Schema.String),
    human_action: Schema.optional(Schema.String),
  }),
  reasons: Schema.NullOr(Schema.Array(Schema.Struct({ code: Schema.String, detail: Schema.String, action: Schema.optional(Schema.String) }))),
  sources: Schema.NullOr(Schema.Array(Schema.Struct({ name: Schema.String, state: Schema.String, code: Schema.optional(Schema.String) }))),
  native_runtime: Schema.optional(Schema.Struct({
    capacity: Schema.NullOr(Schema.Array(Schema.Struct({
      runner_id: Schema.String,
      observed_at: Schema.String,
      available: Schema.Number,
      health: Schema.String,
      exclusions: Schema.NullOr(Schema.Array(Schema.String)),
    }))),
    unavailable: Schema.NullOr(Schema.Array(Schema.String)),
  })),
});
export type IssueExplanation = typeof IssueExplanation.Type;
