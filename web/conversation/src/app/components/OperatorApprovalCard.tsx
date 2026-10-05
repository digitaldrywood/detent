import * as Schema from "effect/Schema";
import React from "react";

import { Button } from "../../components/ui/button.tsx";

const SplitChild = Schema.Struct({
  title: Schema.String,
  description: Schema.String,
  state: Schema.String,
  priority: Schema.optional(Schema.Number),
});
const Split = Schema.Struct({
  parent_work_item_id: Schema.String,
  children: Schema.Array(SplitChild),
  edges: Schema.NullOr(Schema.Array(Schema.Struct({ dependent: Schema.Number, blocker: Schema.Number }))),
});
const Archive = Schema.Struct({
  items: Schema.Array(Schema.Struct({ work_item_id: Schema.String, number: Schema.Number, title: Schema.String, state: Schema.String })),
});
const Approval = Schema.Struct({
  connection_id: Schema.String,
  csrf: Schema.String,
  actions: Schema.Array(Schema.Struct({
    id: Schema.String,
    kind: Schema.String,
    summary: Schema.String,
    status: Schema.Literals(["pending", "succeeded", "failed", "rejected"]),
    description: Schema.String,
    arguments: Schema.Unknown,
    split: Schema.NullOr(Split),
    archive: Schema.NullOr(Archive),
    reason: Schema.String,
    result: Schema.String,
    resource_url: Schema.String,
    form_token: Schema.String,
  })),
});

async function readApproval(url: string, init?: RequestInit): Promise<typeof Approval.Type> {
  const response = await fetch(url, { ...init, credentials: "same-origin", headers: { Accept: "application/json" } });
  if (!response.ok) throw new Error(`The approval could not be loaded or applied (${response.status}). Refresh the preview and try again.`);
  return Schema.decodeUnknownSync(Approval)(await response.json());
}

const STATUS = { pending: "Confirmation required", succeeded: "Executed", failed: "Failed", rejected: "Cancelled" };

export function OperatorApprovalCard({ url, actionID }: { url: string; actionID?: string | undefined }): React.ReactElement {
  const [approval, setApproval] = React.useState<typeof Approval.Type | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);

  React.useEffect(() => {
    const controller = new AbortController();
    void readApproval(url, { signal: controller.signal }).then(setApproval).catch((cause: unknown) => {
      if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "The approval could not be loaded.");
    });
    return () => controller.abort();
  }, [url]);

  async function decide(action: (typeof Approval.Type)["actions"][number], decision: string): Promise<void> {
    if (approval === null) return;
    setBusy(true);
    setError(null);
    try {
      setApproval(await readApproval(url, {
        method: "POST",
        body: new URLSearchParams({ csrf: approval.csrf, form_token: action.form_token, connection_id: approval.connection_id, action_id: action.id, decision }),
      }));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "The approval could not be applied.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="space-y-3">
      {error === null ? null : <p role="alert" className="text-sm">{error}</p>}
      {approval === null && error === null ? <p role="status" className="text-muted-foreground text-sm">Loading approval…</p> : null}
      {approval?.actions.filter((action) => actionID === undefined || action.id === actionID).map((action) => {
        const split = action.split;
        const node = (position: number): string => position === 0 ? "Parent" : `${position}. ${split?.children[position - 1]?.title ?? "Unknown child"}`;
        return (
          <section key={action.id} className="space-y-3">
            <p className="text-muted-foreground text-xs">{STATUS[action.status]}</p>
            <h2 className="break-words font-medium text-sm">{action.summary}</h2>
            {split === null ? null : (
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
            )}
            {action.archive === null ? null : (
              <div data-testid="issue-archive-proposal">
                <p className="text-muted-foreground text-xs">These issues will leave the board and dispatch. They can be restored.</p>
                <ul className="mt-2 space-y-2">{action.archive.items.map((item) => <li key={item.work_item_id} className="text-sm">#{item.number}: {item.title} · {item.state}</li>)}</ul>
              </div>
            )}
            {action.description === "" ? null : <pre className="overflow-auto whitespace-pre-wrap break-words text-xs">{action.description}</pre>}
            {split !== null || action.archive !== null ? null : <pre className="overflow-auto whitespace-pre-wrap break-words text-xs">{JSON.stringify(action.arguments, null, 2)}</pre>}
            {action.reason === "" ? null : <p className="text-muted-foreground text-xs">Reason: {action.reason}</p>}
            {action.result === "" ? null : <p className="text-sm">{action.result}</p>}
            {action.status === "pending" ? (
              <div className="flex gap-2">
                <Button size="sm" variant="outline" disabled={busy} onClick={() => void decide(action, "reject")}>Cancel</Button>
                <Button size="sm" disabled={busy} onClick={() => void decide(action, "confirm")}>Approve</Button>
              </div>
            ) : null}
          </section>
        );
      })}
    </div>
  );
}
