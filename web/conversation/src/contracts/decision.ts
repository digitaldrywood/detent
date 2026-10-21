import * as Schema from "effect/Schema";

export const DecisionRequest = Schema.Struct({
  attempt_id: Schema.String,
  summary: Schema.String,
  question: Schema.String,
  options: Schema.Array(Schema.Struct({
    label: Schema.String,
    consequence: Schema.String,
    recommended: Schema.Boolean,
    return_state: Schema.Literals(["Todo", "Rework"]),
  })),
  custom_allowed: Schema.Boolean,
  custom_return_state: Schema.Literals(["Todo", "Rework"]),
});
export type DecisionRequest = typeof DecisionRequest.Type;

export interface DecisionAnswer {
  readonly attempt_id: string;
  readonly option?: number;
  readonly custom?: string;
}
