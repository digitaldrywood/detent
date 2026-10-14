import { RefreshCwIcon } from "lucide-react";
import React from "react";

import { DiffPanelShell } from "../../../components/DiffPanelShell.tsx";
import { MarkdownCodeBlock } from "../../../components/ChatMarkdown.tsx";
import { Button } from "../../../components/ui/button.tsx";
import {
  Menu,
  MenuItem,
  MenuPopup,
  MenuShortcut,
  MenuTrigger,
} from "../../../components/ui/menu.tsx";
import { cn } from "../../../lib/utils.ts";
import type { WorkspaceReason, WorkspaceState } from "../../../contracts/work.ts";
import {
  OUTPUT_TRUNCATED_MARKER,
  outputText,
  type ActionRunSnapshot,
} from "../../adapters/actionRuns.ts";
import {
  describeWorkspaceStatus,
  type WorkspaceSessionFacts,
} from "../../lib/workspaceStatus.ts";
import { WorkspaceStatusView } from "./WorkspaceStatusView.tsx";

export interface OutputSurfaceProps {
  /** Newest first, as §18.12 lists an action's runs. */
  readonly runs: readonly ActionRunSnapshot[];
  readonly activeRunId: string | null;
  readonly onSelectRun: (runId: string) => void;
  /** Null before the hub has answered at all. */
  readonly state: WorkspaceState | null;
  readonly reason: WorkspaceReason | null;
  /** A request that failed outright, rather than a workspace that failed. */
  readonly error: string | null;
  readonly loading: boolean;
  /** What the session is, for the sentences that name a runner or a time. */
  readonly session?: WorkspaceSessionFacts | null;
  readonly onRetry: () => void;
  /** Re-runs the selected action. Absent where the surface cannot start one. */
  readonly onRerun?: ((run: ActionRunSnapshot) => void) | undefined;
  readonly theme?: "light" | "dark";
}

function EmptyState({
  children,
  testId,
}: {
  children: React.ReactNode;
  testId: string;
}): React.ReactElement {
  return (
    <div
      className="flex items-center justify-center px-3 py-6 text-center text-muted-foreground/70 text-xs"
      data-testid={testId}
    >
      <p>{children}</p>
    </div>
  );
}

export function runStatusLabel(run: ActionRunSnapshot): string {
  switch (run.status) {
    case "connecting":
      return "Connecting";
    case "queued":
      return "Queued";
    case "running":
      return "Running";
    case "succeeded":
      return "Exited 0";
    case "failed":
      return run.exitCode === null ? "Failed" : `Exited ${run.exitCode}`;
  }
}

function RunStatusChip({ run }: { run: ActionRunSnapshot }): React.ReactElement {
  const tone =
    run.status === "succeeded"
      ? "bg-accent text-accent-foreground"
      : run.status === "failed"
        ? "bg-destructive/12 text-destructive"
        : "bg-accent text-accent-foreground";
  return (
    <span
      className={cn(
        "inline-flex h-6 max-w-full shrink-0 items-center gap-1 rounded-md px-2 font-medium text-xs",
        tone,
      )}
      data-testid="output-run-status"
      data-status={run.status}
    >
      <span className="truncate">{runStatusLabel(run)}</span>
    </span>
  );
}

export function OutputSurface({
  runs,
  activeRunId,
  onSelectRun,
  state,
  reason,
  error,
  loading,
  session = null,
  onRetry,
  onRerun,
  theme = "dark",
}: OutputSurfaceProps): React.ReactElement {
  const active = runs.find((run) => run.runId === activeRunId) ?? runs[0] ?? null;

  const header = (
    <>
      <div className="flex min-w-0 flex-1 items-center gap-2">
        {active === null ? (
          <span className="truncate text-muted-foreground/70 text-xs">No runs yet</span>
        ) : (
          <>
            <RunStatusChip run={active} />
            {runs.length > 1 ? (

              <Menu highlightItemOnHover={false}>
                <MenuTrigger
                  render={<Button size="xs" variant="ghost" aria-label="Choose a run" />}
                  data-testid="output-run-picker"
                >
                  <span className="truncate">{active.name}</span>
                </MenuTrigger>
                <MenuPopup align="end">
                  {runs.map((run) => (
                    <MenuItem
                      key={run.runId}
                      data-testid={`output-run-${run.runId}`}
                      onClick={() => onSelectRun(run.runId)}
                    >
                      <span className="truncate">{run.name}</span>
                      <MenuShortcut className="ms-auto">{runStatusLabel(run)}</MenuShortcut>
                    </MenuItem>
                  ))}
                </MenuPopup>
              </Menu>
            ) : (
              <span className="min-w-0 truncate font-medium text-xs">{active.name}</span>
            )}
          </>
        )}
      </div>
      {active !== null && onRerun !== undefined ? (
        <Button
          size="xs"
          variant="ghost"
          aria-label={`Run ${active.name} again`}
          onClick={() => onRerun(active)}
        >
          <RefreshCwIcon className="size-3" />
        </Button>
      ) : null}
    </>
  );

  const body = ((): React.ReactElement => {
    if (error !== null) {
      return (
        <div className="min-h-0 flex-1 overflow-auto">
          <EmptyState testId="output-error">{error}</EmptyState>
          <div className="flex justify-center pb-4">
            <Button size="xs" variant="outline" onClick={onRetry}>
              Try again
            </Button>
          </div>
        </div>
      );
    }
    // A run that has already been recorded is shown even while the workspace
    // it ran in is gone: the record outlives the session (§18.12, "a run
    // nobody watched is still visible"), so the workspace's own state only
    // speaks for a surface with nothing else to say.
    if (active === null) {
      // Nothing asked for yet: the surface has its own thing to say, and no
      // workspace state to report.
      if (state === null && !loading) {
        return (
          <EmptyState testId="output-empty">
            Run a project action from the header to see its output here.
          </EmptyState>
        );
      }
      // Every state that cannot run a command is words (§18.1); the skeleton
      // is kept for `starting` alone, which `describeWorkspaceStatus` decides.
      const status = describeWorkspaceStatus({
        state,
        reason,
        capability: "exec",
        session,
      });
      if (status !== null) {
        return <WorkspaceStatusView status={status} testIdPrefix="output" onRetry={onRetry} />;
      }
      return (
        <EmptyState testId="output-empty">
          Run a project action from the header to see its output here.
        </EmptyState>
      );
    }

    const text = outputText(active);
    return (
      <div className="min-h-0 flex-1 overflow-auto p-2" data-testid="output-run">
        {active.error !== null ? (
          <p className="px-1 pb-2 text-destructive text-xs" data-testid="output-run-error">
            {active.error}
          </p>
        ) : null}

        <div className="chat-markdown">
          <MarkdownCodeBlock
            code={text}
            language="console"
            fenceTitle={active.command}
            theme={theme}
          >
            <pre className="min-w-0 overflow-auto px-3 pt-1 pb-3 font-mono text-[11px] leading-relaxed">
              {text.length > 0
                ? text
                : active.status === "succeeded" || active.status === "failed"
                  ? "(no output)"
                  : ""}
            </pre>
          </MarkdownCodeBlock>
        </div>
        {active.truncated ? (
          // §18.12: the runner emits the marker once, as its own span, so a
          // reader can tell a cut log from a finished one. Said again here as
          // the surface's own line, because the span scrolls away with the
          // output and this does not.
          <p
            className="px-1 pt-2 text-[11px] text-muted-foreground/70"
            data-testid="output-truncated"
          >
            {OUTPUT_TRUNCATED_MARKER} — the run printed more than the 1 MiB the
            hub keeps.
          </p>
        ) : null}
      </div>
    );
  })();

  return (
    <DiffPanelShell mode="sheet" header={header}>
      <div className="flex min-h-0 flex-1 flex-col" data-testid="output-surface">
        {body}
      </div>
    </DiffPanelShell>
  );
}
