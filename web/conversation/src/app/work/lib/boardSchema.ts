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
  backlogTotal: Schema.Number,
});
export type Loaded = typeof LoadedSchema.Type;

export const LiveEntrySchema = Schema.Struct({
  project: NativeProject,
  cards: Schema.Array(NativeBoardCard),
  sequence: Schema.Number,
  grantVersion: Schema.optional(Schema.String),
  work: Schema.optional(NativeWorkSummary),
});

export const BoardViewSchema = Schema.Struct({
  projects: Schema.Array(Schema.Struct({
    project: NativeProject,
    sequence: Schema.Number,
    projection_pending: Schema.Boolean,
    projection_oldest_pending_at: Schema.optional(Schema.String),
    work: Schema.optional(NativeWorkSummary),
  })),
  cards: Schema.Array(NativeBoardCard),
  lanes: Schema.Array(Schema.Struct({ state: Schema.String, next_cursor: Schema.optional(Schema.String) })),
  grant_version: Schema.String,
});
export type BoardView = typeof BoardViewSchema.Type;
