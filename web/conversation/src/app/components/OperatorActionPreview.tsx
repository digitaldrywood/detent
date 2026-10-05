import * as Schema from "effect/Schema";
import React from "react";
import { Markdown } from "./Markdown.tsx";

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

export function issueSplitParent(action: Record<string, unknown>, childCount: number): { identifier: string; title: string; label: string } {
  const identifier = typeof action.identifier === "string" ? action.identifier : "Parent issue";
  const summary = typeof action.title === "string" ? action.title : "";
  const prefix = `Split ${identifier}: `;
  const suffix = ` into ${childCount} issues`;
  const title = summary.startsWith(prefix) && summary.endsWith(suffix) ? summary.slice(prefix.length, -suffix.length) : summary;
  return { identifier, title, label: title === "" ? identifier : `${identifier}: ${title}` };
}

export function OperatorActionPreview({ action }: { action: Record<string, unknown> }): React.ReactElement | null {
  if (action.kind === "propose_issue_split") {
    const parsed = Schema.decodeUnknownOption(Split)(action.arguments);
    if (parsed._tag === "None") return null;
    const split = parsed.value;
    const parent = issueSplitParent(action, split.children.length);
    const node = (position: number): string => position === 0 ? parent.label : `${position}. ${split.children[position - 1]?.title ?? "Unknown child"}`;
    const blockers = (position: number): number[] => (split.edges ?? []).filter((edge) => edge.dependent === position).map((edge) => edge.blocker);
    const parentBlockers = blockers(0);
    const waitsForAll = split.children.length > 0 && split.children.every((_, index) => parentBlockers.includes(index + 1));
    const projectId = typeof action.project_id === "string" ? action.project_id : undefined;
    return (
      <div className="space-y-3" data-testid="issue-split-proposal">
        {split.children.map((child, index) => (
          <section key={index} className="rounded-lg border border-border p-3">
            <h3 className="break-words font-medium text-sm">{index + 1}. {child.title}</h3>
            <p className="mt-1 text-muted-foreground text-xs">{child.state} · {child.priority === undefined ? "No priority" : ["Urgent", "High", "Normal", "Low"][child.priority] ?? "Unknown priority"}</p>
            <Markdown source={child.description} projectId={projectId} className="mt-2 text-xs" />
          </section>
        ))}
        <div role="group" aria-label="Dependency graph" className="text-muted-foreground text-xs">
          <p className="font-medium">Dependency graph</p>
          {split.children.map((_, index) => <p key={index}>{node(index + 1)} — Blocked by: {blockers(index + 1).map(node).join(", ") || "None"}</p>)}
          <p>{waitsForAll ? `${parent.identifier} waits for all ${split.children.length} children${parent.title === "" ? "" : ` · ${parent.title}`}` : `Parent: ${parent.label}${parentBlockers.length === 0 ? "" : ` — Blocked by: ${parentBlockers.map(node).join(", ")}`}`}</p>
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
