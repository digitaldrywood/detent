import * as Schema from "effect/Schema";
export const SavedWorkState = Schema.Struct({
  view: Schema.Literals(["board", "list"]),
  tab: Schema.Literals(["active", "backlog", "closed", "all"]),
  q: Schema.String,
  sort: Schema.Literals([
    "default",
    "priority",
    "updated",
    "created",
    "title",
    "closed",
  ]),
  completedWindow: Schema.Literals(["1h", "24h", "7d"]),
  completedFrom: Schema.optional(Schema.String),
  completedTo: Schema.optional(Schema.String),
  archived: Schema.optional(Schema.Boolean),
  createdBy: Schema.optional(Schema.Literal("me")),
  state: Schema.Array(Schema.String),
  type: Schema.Array(Schema.String),
  label: Schema.Array(Schema.String),
  assignee: Schema.Array(Schema.String),
  priority: Schema.Array(Schema.String),
  lanes: Schema.NullOr(Schema.Array(Schema.String)),
  collapsed: Schema.Array(Schema.String),
});
export const SavedWorkView = Schema.Struct({
  can_edit: Schema.Boolean,
  id: Schema.String,
  owner_id: Schema.String,
  name: Schema.String,
  scope: Schema.Literals(["personal", "organization"]),
  project_id: Schema.NullOr(Schema.String),
  favorite: Schema.Boolean,
  state: SavedWorkState,
});
export type SavedView = typeof SavedWorkView.Type;
export type SavedViewInput = Omit<SavedView, "id" | "owner_id" | "can_edit">;
