import * as Schema from "effect/Schema";
import { NativeIssue, NativeProject, NativeWorkSummary, NativeBoardCard } from "../../../contracts/work.ts";

export const LoadedSchema = Schema.Struct({
  project: NativeProject,
  issues: Schema.Array(NativeIssue),
  cards: Schema.optional(Schema.Array(NativeBoardCard)),
  sequence: Schema.optional(Schema.Number),
  grantVersion: Schema.optional(Schema.String),
  work: Schema.optional(NativeWorkSummary),
  nextCursor: Schema.optional(Schema.String),
  pageCount: Schema.Number,
  backlogCursor: Schema.optional(Schema.String),
  backlogPageCount: Schema.Number,
  backlogTotal: Schema.Number,
});
export type Loaded = typeof LoadedSchema.Type;
