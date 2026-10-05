import * as Schema from "effect/Schema";
import React from "react";

const Split = Schema.Struct({
  parent_work_item_id: Schema.String,
  children: Schema.Array(Schema.Struct({
    title: Schema.String,
    description: Schema.String,
    state: Schema.String,
    priority: Schema.optional(Schema.Number),
  })),
  edges: Schema.NullOr(Schema.Array(Schema.Struct({ dependent: Schema.Number, blocker: Schema.Number }))),
});
const Archive = Schema.Struct({
  items: Schema.Array(Schema.Struct({
    work_item_id: Schema.String,
    number: Schema.Number,
    title: Schema.String,
    state: Schema.String,
  })),
});

export function OperatorActionPreview({ action }: { action: Record<string, unknown> }): React.ReactElement | null {
  if (action.kind === "propose_issue_split") {
    const parsed = Schema.decodeUnknownOption(Split)(action.arguments);
    if (parsed._tag === "None") return null;
    const split = parsed.value;
    const node = (position: number): string => position === 0 ? "Parent" : `${position}. ${split.children[position - 1]?.title ?? "Unknown child"}`;
    return (
      <div className="space-y-3" data-testid="issue-split-proposal">
        {split.children.map((child, index) => (
          <section key={index} className="rounded-lg border border-border p-3">
            <h3 className="break-words font-medium text-sm">{index + 1}. {child.title}</h3>
            <p className="mt-1 text-muted-foreground text-xs">{child.state} · {child.priority === undefined ? "No priority" : ["Urgent", "High", "Normal", "Low"][child.priority] ?? "Unknown priority"}</p>
            <p className="mt-2 whitespace-pre-wrap break-words text-xs">{child.description}</p>
          </section>
        ))}
        <div role="group" aria-label="Dependency graph" className="text-muted-foreground text-xs">
          <p className="font-medium">Dependency graph</p>
          <p>Parent: {split.parent_work_item_id}</p>
          {(split.edges ?? []).map((edge, index) => <p key={index}>{node(edge.dependent)} → blocked by → {node(edge.blocker)}</p>)}
          {(split.edges?.length ?? 0) === 0 ? <p>No dependencies</p> : null}
        </div>
      </div>
    );
  }
  if (action.kind === "archive_items") {
    const parsed = Schema.decodeUnknownOption(Archive)(action.arguments);
    if (parsed._tag === "None") return null;
    return (
      <div data-testid="issue-archive-proposal">
        <p className="text-muted-foreground text-xs">These issues will leave the board and dispatch. They can be restored.</p>
        <ul className="mt-2 space-y-2">{parsed.value.items.map((item) => <li key={item.work_item_id} className="text-sm">#{item.number}: {item.title} · {item.state}</li>)}</ul>
      </div>
    );
  }
  return null;
}
