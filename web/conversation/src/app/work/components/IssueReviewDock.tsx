import { Tabs } from "@base-ui/react/tabs";
import React from "react";

import type { ChangeDetail } from "../../../contracts/work.ts";
import { useTheme } from "../../adapters/theme.ts";
import { AttemptDiffBody } from "../../components/surfaces/DiffSurface.tsx";
import { currentVersion, useRoundDiff } from "../lib/roundDiff.ts";
import { useWorkHttp } from "../lib/useWork.ts";

interface IssueChange {
  readonly record: { readonly change_id: string; readonly title: string };
  readonly detail: ChangeDetail | null;
}

function ChangeDiff({
  change,
  projectId,
  workItemId,
  onOpen,
}: {
  readonly change: IssueChange;
  readonly projectId: string;
  readonly workItemId: string;
  readonly onOpen: (changeId: string) => void;
}): React.ReactElement {
  const http = useWorkHttp();
  const { resolvedTheme } = useTheme();
  const version = change.detail === null ? null : currentVersion(change.detail);
  const { diff, loading } = useRoundDiff(http, projectId, workItemId, version);
  const status = change.detail?.summary.status;
  const landed = status === "landed" || status === "merged";
  const mergeSha = change.detail?.external_snapshot?.merge_commit_sha;
  return (
    <section data-testid="issue-review-change" data-change-id={change.record.change_id}>
      <div className="dock-sub border-b border-border px-3 py-2 text-xs">
        <button
          className="text-left font-medium text-primary hover:underline"
          onClick={() => onOpen(change.record.change_id)}
        >
          {change.record.title}
        </button>
        <p className="mt-1 break-all font-mono text-muted-foreground">
          {version === null
            ? "No published round"
            : `Round ${version.number} · head ${version.head_sha.slice(0, 7)}`}
          {landed
            ? ` · Landed${typeof mergeSha === "string" ? ` ${mergeSha.slice(0, 7)}` : ""}`
            : ""}
        </p>
        <button
          className="mt-1 text-primary hover:underline"
          onClick={() => onOpen(change.record.change_id)}
        >
          Round history and review
        </button>
      </div>
      {diff === null ? (
        <p
          className="px-3 py-6 text-center text-xs text-muted-foreground"
          data-testid="issue-review-diff-empty"
        >
          {change.detail === null
            ? "This change’s details are unavailable. Open the change to try again."
            : loading
              ? "Reading the diff…"
              : version === null
                ? "This change has no published version, so there is no diff to show."
                : "No stored diff is available for this round."}
        </p>
      ) : (
        <AttemptDiffBody diff={diff} theme={resolvedTheme} />
      )}
    </section>
  );
}

export function IssueReviewDock({
  changes,
  projectId,
  workItemId,
  properties,
  activity,
  onOpenChange,
}: {
  readonly changes: readonly IssueChange[];
  readonly projectId: string;
  readonly workItemId: string;
  readonly properties: React.ReactNode;
  readonly activity: React.ReactNode;
  readonly onOpenChange: (changeId: string) => void;
}): React.ReactElement {
  return (
    <Tabs.Root
      defaultValue="diff"
      className="flex min-h-0 flex-1 flex-col"
      data-testid="issue-review-dock"
    >
      <Tabs.List
        className="flex h-[52px] shrink-0 items-center gap-1 border-b border-border px-3"
        aria-label="Issue review"
      >
        {["Diff", "State", "Receipt", "Activity"].map((tab) => (
          <Tabs.Tab
            key={tab}
            value={tab.toLowerCase()}
            className="rounded-md px-3 py-1.5 text-xs text-muted-foreground data-[active]:bg-accent data-[active]:text-foreground"
          >
            {tab}
          </Tabs.Tab>
        ))}
      </Tabs.List>
      <Tabs.Panel value="diff" className="min-h-0 flex-1 overflow-y-auto">
        {changes.length === 0 ? (
          <>
            <p
              className="px-3 py-6 text-center text-xs text-muted-foreground"
              data-testid="issue-review-empty"
            >
              No change requests yet. Published changes will appear here.
            </p>
            <div className="px-6 pb-8">{properties}</div>
          </>
        ) : (
          changes.map((change) => (
            <ChangeDiff
              key={change.record.change_id}
              change={change}
              projectId={projectId}
              workItemId={workItemId}
              onOpen={onOpenChange}
            />
          ))
        )}
      </Tabs.Panel>
      <Tabs.Panel value="state" className="min-h-0 flex-1 overflow-y-auto px-6 py-8">
        {properties}
      </Tabs.Panel>
      <Tabs.Panel value="receipt" className="min-h-0 flex-1 overflow-y-auto p-3">
        {changes.length === 0 ? (
          <p className="text-xs text-muted-foreground">No change receipts yet.</p>
        ) : (
          changes.map(({ record, detail }) => (
            <section
              key={record.change_id}
              className="mb-3 rounded-lg border border-border p-3 text-xs"
            >
              <button
                className="font-medium text-primary hover:underline"
                onClick={() => onOpenChange(record.change_id)}
              >
                {record.title}
              </button>
              <p className="mt-2">
                {detail?.summary.status.replaceAll("_", " ") ?? "Details unavailable"}
              </p>
              {detail === null ? null : (
                <>
                  <p className="mt-1 text-muted-foreground">
                    Native review: {detail.summary.native_review} · External review:{" "}
                    {detail.summary.external_review} · Checks: {detail.summary.checks}
                  </p>
                  {detail.summary.messages.map((message) => (
                    <p key={message} className="mt-1">
                      {message}
                    </p>
                  ))}
                </>
              )}
            </section>
          ))
        )}
      </Tabs.Panel>
      <Tabs.Panel value="activity" className="min-h-0 flex-1 overflow-y-auto p-3">
        {activity}
      </Tabs.Panel>
    </Tabs.Root>
  );
}
