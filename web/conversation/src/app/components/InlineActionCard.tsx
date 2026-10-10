import React from "react";
import { Button } from "../../components/ui/button.tsx";
import { useClient } from "../client.ts";
import { issueSplitParent, OperatorActionPreview } from "./OperatorActionPreview.tsx";

export function InlineActionCard({ proposal, text }: {
  proposal: { readonly action: Record<string, unknown>; readonly conversation_id: string };
  text: string;
}): React.ReactElement {
  const client = useClient();
  const requiresApproval = proposal.action.kind === "set_runner_tier" || proposal.action.kind === "set_sprite_pool";
  const preferenceKey = `detent:chat-confirmation:${client.bootstrap.actor.principal_id}`;
  const resultKey = `${preferenceKey}:${String(proposal.action.request_id)}`;
  const requiresConfirmation = requiresApproval || proposal.action.requires_confirmation === true;
  const [confirm, setConfirm] = React.useState(() => {
    try { return requiresConfirmation || localStorage.getItem(preferenceKey) !== "off"; } catch { return true; }
  });
  const [status, setStatus] = React.useState(() => {
    try { return localStorage.getItem(resultKey) ?? "proposed"; } catch { return "proposed"; }
  });
  const [error, setError] = React.useState("");
  const submitting = React.useRef(false);
  const execute = React.useCallback(async () => {
    if (submitting.current || status !== "proposed") return;
    submitting.current = true;
    setStatus("running");
    try {
      const response = await fetch(`${client.bootstrap.api_base}/projects/${encodeURIComponent(String(proposal.action.project_id))}/conversations/${encodeURIComponent(proposal.conversation_id)}/actions`, {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": client.bootstrap.csrf_token },
        body: JSON.stringify(proposal.action),
      });
      if (!response.ok) throw new Error(`The change could not be applied (${response.status}).`);
      setStatus("completed");
      try { localStorage.setItem(resultKey, "completed"); } catch { }
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "The change could not be applied.");
      setStatus("failed");
    } finally { submitting.current = false; }
  }, [client, proposal, resultKey, status]);
  React.useEffect(() => { if (!requiresConfirmation && !confirm && status === "proposed") void execute(); }, [requiresConfirmation, confirm, status, execute]);
  const argumentsValue = proposal.action.arguments;
  const children = typeof argumentsValue === "object" && argumentsValue !== null && "children" in argumentsValue ? argumentsValue.children : undefined;
  const change = typeof argumentsValue === "object" && argumentsValue !== null
    ? Object.fromEntries(Object.entries(argumentsValue)
      .filter(([key]) => !["project_id", "request_id", "identifier", "expected_revision"].includes(key))
      .map(([key, value]) => [key, proposal.action.kind === "propose_issue_split" && key === "parent_work_item_id"
        ? issueSplitParent(proposal.action, Array.isArray(children) ? children.length : 0).label
        : value]))
    : argumentsValue;
  return <section className="rounded-lg border p-3 space-y-3" data-testid="operator-action-card">
    <p className="text-sm">{text}</p>
    <OperatorActionPreview action={proposal.action} />
    <details><summary className="text-xs cursor-pointer">Proposed change</summary><pre className="whitespace-pre-wrap break-all text-xs">{JSON.stringify(change, null, 2)}</pre></details>
    {requiresApproval ? null : <label className="flex items-center gap-2 text-xs"><input type="checkbox" disabled={requiresConfirmation} checked={requiresConfirmation || confirm} onChange={(event) => {
      const enabled = event.target.checked;
      setConfirm(enabled);
      try { localStorage.setItem(preferenceKey, enabled ? "on" : "off"); } catch { }
    }} />Ask me to confirm chat changes</label>}
    {error === "" ? null : <p role="alert" className="text-xs">{error}</p>}
    {status === "proposed" ? <div className="flex justify-end gap-2">
      <Button variant="outline" size="sm" onClick={() => {
        setStatus("cancelled");
        try { localStorage.setItem(resultKey, "cancelled"); } catch { }
      }}>Cancel</Button>
      <Button size="sm" onClick={() => { void execute(); }}>Approve</Button>
    </div> : <p role="status" className="text-xs">{status === "running" ? "Applying change…" : status === "completed" ? "Change completed." : status === "cancelled" ? "Cancelled." : "Change failed. Request a fresh proposal."}</p>}
  </section>;
}
