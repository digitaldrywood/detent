import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { cn } from "../../lib/utils.ts";

import type { Execution } from "../../contracts/index.ts";
import type { PendingControl } from "../../runtime/state/conversationState.ts";
import {
  canContinue,
  canInterrupt,
  canStopChat,
  executionCopy,
  executionStripSentence,
  type ExecutionSurface,
} from "../lib/execution.ts";

const STRIP_ROW =
  "flex min-h-7 w-full min-w-0 items-center gap-1 ps-1 pe-2 font-normal text-muted-foreground/70 text-xs sm:min-h-6";

export interface ExecutionStripProps {
  readonly execution: Execution;
  /** The interrupt and continue this client sent, if they are unsettled. */
  readonly controls: readonly PendingControl[];
  readonly onInterrupt: () => void;
  readonly onContinue: () => void;
  readonly onRetryControl: (entry: PendingControl) => void;
  readonly onDiscardControl: (entry: PendingControl) => void;
  /** A control came back `stale_execution`: the snapshot has been re-read. */
  readonly stale: boolean;
  readonly onDismissStale: () => void;
  /** Stated reason the controls are unavailable, e.g. a read-only project. */
  readonly blockedReason?: string | null;
  /** `chat` is an unlinked conversation: chat vocabulary, one Stop control. */
  readonly surface?: ExecutionSurface;
  /** Named by the chat strip while it waits for a runner in that project. */
  readonly projectName?: string | null;
  /** Sends `cancel`. Required by the chat surface, unused by the issue one. */
  readonly onStop?: () => void;
  /**
   * A viewer who cannot write, or an archived chat. Controls are not rendered
   * at all rather than rendered disabled: a disabled button reads as "not yet"
   * and this reader is never going to press it (decisions.md §10.11, §10.12).
   */
  readonly readOnly?: boolean;
}

function ControlReceipt({
  entry,
  onRetry,
  onDiscard,
}: {
  entry: PendingControl;
  onRetry: (entry: PendingControl) => void;
  onDiscard: (entry: PendingControl) => void;
}): React.ReactElement | null {
  if (entry.status === "sending" || entry.status === "sent") return null;
  const verb = entry.kind === "interrupt" ? "Interrupt" : "Continue";
  return (
    <div className={cn(STRIP_ROW, "gap-2")} data-testid="control-receipt">
      <span
        className={cn(
          "size-1.5 shrink-0 rounded-full",
          entry.status === "rejected" ? "bg-error" : "bg-warning",
        )}
        aria-hidden="true"
      />
      <span className="min-w-0 flex-1 truncate">
        <b>
          {verb} {entry.status}
        </b>
        {entry.status === "unknown"
          ? " · We could not confirm the runner received this. Retry sends the same command once."
          : entry.error === null
            ? ""
            : ` · ${entry.error}`}
      </span>
      <span className="ml-auto" />
      {entry.status === "unknown" ? (
        <Button variant="ghost" size="xs" onClick={() => onRetry(entry)}>
          Retry same command
        </Button>
      ) : null}
      <Button variant="ghost" size="xs" onClick={() => onDiscard(entry)}>
        Discard
      </Button>
    </div>
  );
}

const TONE_FILL: Record<string, string> = {
  "dc-ok": "bg-success",
  "dc-warn": "bg-warning",
  "dc-err": "bg-error",
  "dc-info": "bg-info",
  "": "bg-muted-foreground/60",
};

export function ExecutionStrip(props: ExecutionStripProps): React.ReactElement {
  const { execution } = props;
  const chat = props.surface === "chat";
  const copyOptions = {
    ...(chat ? { surface: "chat" as const } : {}),
    ...(props.projectName == null ? {} : { projectName: props.projectName }),
  };
  const copy = executionCopy(execution, copyOptions);
  const sentence = executionStripSentence(execution, copyOptions);
  const readOnly = props.readOnly === true;
  const blocked = props.blockedReason != null || readOnly;
  const interruptable = canInterrupt(execution) && !blocked;
  const continuable = canContinue(execution) && !blocked;
  const stoppable = canStopChat(execution) && !blocked;

  return (
    <div className="flex w-full min-w-0 flex-col gap-1">
      {props.stale ? (
        <div
          className="flex items-center gap-2 rounded-md bg-warning-surface px-2 py-1 text-warning-foreground text-xs"
          role="alert"
          data-testid="stale-execution"
        >
          <span>The runner changed; review the new state.</span>
          <span className="ml-auto" />
          <Button variant="ghost" size="xs" onClick={props.onDismissStale}>
            Dismiss
          </Button>
        </div>
      ) : null}

      {props.controls.map((entry) => (
        <ControlReceipt
          key={entry.key}
          entry={entry}
          onRetry={props.onRetryControl}
          onDiscard={props.onDiscardControl}
        />
      ))}

      {/* Transcript recovery is visible: this attempt was handed the last few
          messages rather than the provider thread, and a reader who assumed
          the earlier context was there would read the answer wrong
          (decisions.md §10.4). */}
      {execution.resume === "transcript" ? (
        <p
          className="ps-1 pe-2 font-normal text-muted-foreground/70 text-xs"
          role="status"
          data-testid="execution-resume"
        >
          This runner continued from a transcript of recent messages; the provider&apos;s
          earlier context was not available.
        </p>
      ) : null}

      <div className={STRIP_ROW} data-testid="execution-strip">
        <span
          className={cn("size-1.5 shrink-0 rounded-full", TONE_FILL[copy.tone] ?? TONE_FILL[""])}
          aria-hidden="true"
        />
        {/* The status is the thing that changes under the reader, so it is the
            live region; the controls beside it are not announced. One line,
            truncated: the strip sits on the composer card and a second line
            pushes the transcript up every time the status changes. */}
        <span
          className="min-w-0 flex-1 truncate"
          aria-live="polite"
          data-testid="execution-copy"
        >
          <b className="font-medium text-foreground">{copy.label}</b>
          {sentence === null ? null : ` · ${sentence}`}
        </span>
        {execution.attempt_id === null ? null : (
          <span
            className="min-w-0 max-w-48 shrink-0 truncate font-mono text-muted-foreground/70 tabular-nums"
            data-testid="execution-attempt"
          >
            attempt {execution.attempt_id}
          </span>
        )}
        <span className="ml-auto" />
        {chat && !readOnly ? (
          stoppable ? (
            <Button
              variant="outline"
              size="xs"
              onClick={props.onStop}
              title="Stop the runner answering this chat."
            >
              Stop
            </Button>
          ) : null
        ) : null}
        {!chat && !readOnly && execution.capabilities.interrupt ? (
          <Button
            variant="outline"
            size="xs"
            onClick={props.onInterrupt}
            disabled={!interruptable}
            aria-disabled={!interruptable}
            title={
              interruptable
                ? `Stop the current attempt (${execution.attempt_id ?? "no attempt"})`
                : (props.blockedReason ?? "There is no running attempt to interrupt.")
            }
          >
            Interrupt
          </Button>
        ) : null}
        {!chat && !readOnly && execution.capabilities.continue ? (
          <Button
            variant="outline"
            size="xs"
            onClick={props.onContinue}
            disabled={!continuable}
            aria-disabled={!continuable}
            title={
              continuable
                ? "Ask the scheduler for another attempt."
                : (props.blockedReason ?? "Continue is available once the current attempt ends.")
            }
          >
            Continue
          </Button>
        ) : null}
      </div>
    </div>
  );
}
