import * as Exit from "effect/Exit";
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

export const BoardViewWire = Schema.Struct({
  projects: Schema.Array(Schema.Struct({
    project: NativeProject,
    sequence: Schema.Number,
    projection_pending: Schema.Boolean,
    projection_oldest_pending_at: Schema.optional(Schema.String),
    work: Schema.optional(NativeWorkSummary),
  })),
  cards: Schema.Array(Schema.Unknown),
  lanes: Schema.Array(Schema.Struct({ state: Schema.String, next_cursor: Schema.optional(Schema.String) })),
  grant_version: Schema.String,
});

export interface BoardView extends Omit<typeof BoardViewWire.Type, "cards"> {
  readonly cards: readonly NativeBoardCard[];
}

const decodeCard = Schema.decodeUnknownExit(NativeBoardCard);

export const BOARD_CARD_DROP_TOLERANCE = 0.05;

export function decodeBoardView(wire: typeof BoardViewWire.Type, options: { readonly partial?: boolean } = {}): BoardView {
  const cards: NativeBoardCard[] = [];
  for (const raw of wire.cards) {
    const card = decodeCard(raw);
    if (Exit.isSuccess(card)) cards.push(card.value);
    else {
      const issue = typeof raw === "object" && raw !== null && "issue" in raw ? raw.issue : undefined;
      const id = typeof issue === "object" && issue !== null && "work_item_id" in issue ? issue.work_item_id : undefined;
      console.error("Dropped a board card that does not match the contract", { work_item_id: id, cause: card.cause });
    }
  }
  const dropped = wire.cards.length - cards.length;
  if (options.partial !== true && dropped > 0 && (dropped === wire.cards.length || dropped > Math.max(1, wire.cards.length * BOARD_CARD_DROP_TOLERANCE))) {
    throw new Error(`The board could not be shown: ${dropped} of ${wire.cards.length} cards do not match this version of the app.`);
  }
  return { ...wire, cards };
}
