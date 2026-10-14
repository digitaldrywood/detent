import React from "react";

import type { ObservedPolicy, PolicyApproval, PolicyDescriptor } from "../../contracts/account.ts";
import { Button } from "../../components/ui/button.tsx";
import { SettingsRow } from "../settings/settingsLayout.tsx";
import { ControlError } from "./controls.tsx";
import { workflowChanges } from "./workflowDiff.ts";

function DefinitionDiff({ before, after, unavailable }: {
  readonly before?: PolicyDescriptor;
  readonly after?: PolicyDescriptor;
  readonly unavailable?: boolean;
}): React.ReactElement {
  if (!after || unavailable) {
    return <p className="pb-3 text-sm text-muted-foreground">The stored definition for this comparison is unavailable.</p>;
  }
  const changes = workflowChanges(before, after);
  if (changes.length === 0) {
    return <p className="pb-3 text-sm text-muted-foreground">No lane, transition or scheduling changes.</p>;
  }
  return (
    <div className="overflow-x-auto pb-3">
      <table className="w-full table-fixed text-left text-xs [overflow-wrap:anywhere]" aria-label="Workflow revision changes">
        <thead>
          <tr className="border-b border-border">
            <th scope="col" className="w-1/4 p-2">Setting</th>
            <th scope="col" className="p-2">Before</th>
            <th scope="col" className="p-2">After</th>
          </tr>
        </thead>
        <tbody>
          {changes.map((change) => (
            <tr key={change.field} className="border-b border-border/50 align-top">
              <th scope="row" className="p-2 font-medium">{change.field}</th>
              <td className="p-2 text-muted-foreground">{change.before}</td>
              <td className="p-2">{change.after}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function WorkflowRevisions({
  policy,
  observed,
  repository,
  canApprove,
  approving,
  onApprove,
  error,
  onLoadOlder,
  loadingOlder,
}: {
  readonly policy: PolicyApproval | null;
  readonly observed: readonly ObservedPolicy[];
  readonly repository: string;
  readonly canApprove: boolean;
  readonly approving: boolean;
  readonly onApprove: (policyId: string) => void;
  readonly error: string | null;
  readonly onLoadOlder: () => void;
  readonly loadingOlder: boolean;
}): React.ReactElement {
  const entries = [
    ...(policy?.history ?? []).map((entry) => ({
      key: `applied-${entry.id}`,
      time: entry.applied_at,
      repository: entry.repository,
      commit: entry.commit,
      actor: entry.applied_by,
      before: entry.previous_definition,
      after: entry.definition,
      unavailable: entry.previous_definition_digest !== "" && !entry.previous_definition,
      pending: false,
      policyId: "",
    })),
    ...observed.filter((entry) => entry.policy.workflow && !entry.previously_approved).map((entry) => ({
      key: `pending-${entry.policy.policy_id}`,
      time: entry.observed_at,
      repository: entry.repository_source?.repository || repository,
      commit: entry.repository_source?.commit || entry.policy.workflow?.revision || "",
      actor: "",
      before: policy?.policy,
      after: entry.policy,
      unavailable: false,
      pending: true,
      policyId: entry.policy.policy_id,
    })),
  ].sort((a, b) => Date.parse(b.time) - Date.parse(a.time) || b.time.localeCompare(a.time));
  return (
    <>
      {entries.map((entry) => (
        <SettingsRow
          key={entry.key}
          title={entry.pending ? "Pending revision" : "Applied revision"}
          description={
            <span>{entry.repository || "Repository not recorded"} · <span className="font-mono">{entry.commit || "Commit not recorded"}</span></span>
          }
          status={entry.pending
            ? <span>Awaiting approval · Observed at <time dateTime={entry.time}>{entry.time}</time></span>
            : <span>Applied by {entry.actor} at <time dateTime={entry.time}>{entry.time}</time></span>}
          control={entry.pending && canApprove ? (
            <Button
              size="xs"
              disabled={approving}
              aria-label={`Approve updated policy ${entry.policyId}`}
              onClick={() => onApprove(entry.policyId)}
            >
              {approving ? "Approving…" : "Approve updated policy"}
            </Button>
          ) : undefined}
        >
          <DefinitionDiff before={entry.before} after={entry.after} unavailable={entry.unavailable} />
        </SettingsRow>
      ))}
      <ControlError message={error} />
      {policy?.history_next ? (
        <div className="py-3">
          <Button size="sm" variant="ghost-muted" disabled={loadingOlder} onClick={onLoadOlder}>
            {loadingOlder ? "Loading…" : "Load older revisions"}
          </Button>
        </div>
      ) : null}
    </>
  );
}
