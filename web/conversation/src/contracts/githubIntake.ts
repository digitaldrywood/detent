import * as Schema from "effect/Schema";

export const IssueIntake = Schema.Struct({
  batch: Schema.NullOr(Schema.Struct({
    id: Schema.String,
    revision: Schema.Number,
    runner_id: Schema.String,
    status: Schema.String,
    error: Schema.optional(Schema.String),
    retry_at: Schema.optional(Schema.String),
    destination: Schema.String,
    discovery: Schema.Struct({ repository: Schema.String, include_closed: Schema.Boolean, labels: Schema.NullOr(Schema.Array(Schema.String)), cursor: Schema.String }),
    page: Schema.Struct({
      total: Schema.Number,
      next_cursor: Schema.String,
      issues: Schema.Array(Schema.Struct({ number: Schema.Number, id: Schema.String, url: Schema.String, title: Schema.String, body: Schema.String, closed: Schema.Boolean, labels: Schema.Array(Schema.String) })),
    }),
    items: Schema.Array(Schema.Struct({ number: Schema.Number, work_item_id: Schema.optional(Schema.String), status: Schema.String, error: Schema.optional(Schema.String), retry_at: Schema.optional(Schema.String) })),
  })),
  lanes: Schema.Array(Schema.Struct({ name: Schema.String, dispatchable: Schema.Boolean, terminal: Schema.Boolean })),
});
export type IssueIntake = typeof IssueIntake.Type;
export interface IntakeCommand {
  readonly action: "discover" | "more" | "apply" | "retry";
  readonly revision: number;
  readonly runner_id?: string;
  readonly labels?: readonly string[];
  readonly include_closed?: boolean;
  readonly numbers?: readonly number[];
  readonly destination?: string;
  readonly allow_dispatch?: boolean;
}
