import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { cn } from "../../lib/utils.ts";
import type { PendingMessage } from "../../runtime/state/conversationState.ts";
import type { ExecutionSurface } from "../lib/execution.ts";
import { DeliveryChip } from "./DeliveryChip.tsx";
import { RETRY_COPY } from "./Timeline.tsx";

export function PendingRow({
  entry,
  surface,
  onRetry,
  onDismiss,
}: {
  entry: PendingMessage;
  surface: ExecutionSurface;
  onRetry: (entry: PendingMessage) => void;
  onDismiss: (entry: PendingMessage) => void;
}): React.ReactElement {
  return (
    <div className="group flex flex-col items-end gap-1" data-testid="pending-turn">
      <span className="sr-only">You · not yet confirmed</span>
      <div
        className={cn(
          "relative max-w-[80%] whitespace-pre-wrap rounded-2xl bg-message p-3 text-message-foreground text-sm",
          entry.status === "sending" && "opacity-70",
        )}
      >
        {entry.text}
      </div>
      <div className="flex items-center gap-2 text-muted-foreground text-xs">
        <DeliveryChip
          status={entry.status}
          detail={entry.error}
          errorCode={entry.errorCode}
          surface={surface}
        />
        {entry.status === "sending" ? null : (
          <>
            <Button variant="ghost" size="xs" title={RETRY_COPY} onClick={() => onRetry(entry)}>
              Retry
            </Button>
            <Button variant="ghost" size="xs" onClick={() => onDismiss(entry)}>
              Dismiss retry
            </Button>
          </>
        )}
      </div>
      {entry.status === "unknown" ? (
        <div
          className="flex flex-col gap-1 rounded-md bg-warning-surface px-2 py-1 text-warning-foreground text-xs"
          role="status"
        >
          <span>Delivery is uncertain. Recover history before deciding whether to send again.</span>
          <span>{RETRY_COPY}</span>
        </div>
      ) : null}
    </div>
  );
}
