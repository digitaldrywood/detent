import React from "react";

import { Spinner } from "../../components/ui/spinner.tsx";

export interface ComposerPrimaryActionsProps {
  /** True while a turn is producing output: stop replaces send. */
  readonly isRunning: boolean;
  readonly onInterrupt?: (() => void) | undefined;
  /** True while the send round trip is in flight. */
  readonly isSendBusy: boolean;
  /** Stated reason the send is unavailable; `null` means it is available. */
  readonly sendDisabledReason: string | null;
  readonly hasSendableContent: boolean;
  readonly onSend: () => void;
}

/**
 * Stop is a circle, not a square button: it is the one destructive control in
 * the composer and reads as its own affordance rather than as a variant of
 * send.
 */
function StopButton({ onInterrupt }: { onInterrupt: () => void }): React.ReactElement {
  return (
    <button
      type="button"
      className="flex size-8 cursor-pointer items-center justify-center rounded-full bg-destructive/90 text-white shadow-xs shadow-destructive/24 inset-shadow-[0_1px_--theme(--color-white/16%)] transition-all duration-150 hover:scale-105 hover:bg-destructive active:inset-shadow-[0_1px_--theme(--color-black/8%)] active:shadow-none sm:h-8 sm:w-8"
      onClick={onInterrupt}
      aria-label="Stop the current turn"
    >
      <svg width="12" height="12" viewBox="0 0 12 12" fill="currentColor" aria-hidden="true">
        <rect x="2" y="2" width="8" height="8" rx="1.5" />
      </svg>
    </button>
  );
}

export function ComposerPrimaryActions(props: ComposerPrimaryActionsProps): React.ReactElement {
  const disabled =
    props.isSendBusy || props.sendDisabledReason !== null || !props.hasSendableContent;

  if (props.isRunning && props.onInterrupt !== undefined) {
    return <StopButton onInterrupt={props.onInterrupt} />;
  }

  return (
    <button
      type="button"
      className="relative isolate flex h-9 w-9 items-center justify-center overflow-hidden rounded-full bg-message-action text-message-action-foreground shadow-xs transition-all duration-150 enabled:cursor-pointer enabled:inset-shadow-[0_1px_--theme(--color-white/16%)] enabled:shadow-message-action/24 hover:scale-105 hover:bg-message-action-hover active:inset-shadow-[0_1px_--theme(--color-black/8%)] active:shadow-none disabled:pointer-events-none disabled:opacity-30 disabled:shadow-none disabled:hover:scale-100 sm:h-8 sm:w-8"
      onClick={() => {
        if (!disabled) props.onSend();
      }}
      disabled={disabled}
      aria-disabled={disabled}
      aria-label="Send message"
    >
      {props.isSendBusy ? (
        <Spinner className="size-3.5" aria-hidden="true" />
      ) : (
        <svg width="14" height="14" viewBox="0 0 14 14" fill="none" aria-hidden="true">
          <path
            d="M7 11.5V2.5M7 2.5L3 6.5M7 2.5L11 6.5"
            stroke="currentColor"
            strokeWidth="1.8"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      )}
    </button>
  );
}
