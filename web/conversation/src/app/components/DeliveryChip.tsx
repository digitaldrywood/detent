import React from "react";

import { cn } from "../../lib/utils.ts";

import type { DeliveryStatus } from "../../contracts/index.ts";
import type { ExecutionSurface } from "../lib/execution.ts";

export type ChipStatus = DeliveryStatus | "sending" | "queued" | "unknown" | "failed";

interface Copy {
  readonly label: string;
  readonly tone: "dc-ok" | "dc-warn" | "dc-err" | "dc-info" | "";
  readonly explanation: string;
}

const COPY: Record<string, Copy> = {
  saved: {
    label: "saved",
    tone: "",
    explanation: "The hub stored this message. It has not reached a runner yet.",
  },
  queued: {
    label: "queued",
    tone: "dc-warn",
    explanation: "Waiting for a bound runner to pick the message up.",
  },
  sending: {
    label: "sending",
    tone: "dc-warn",
    explanation: "Handed to the runner. No confirmation yet.",
  },
  sent: {
    label: "sent",
    tone: "",
    explanation: "The runner confirmed receipt, before the provider write.",
  },
  delivered: {
    label: "delivered",
    tone: "dc-info",
    explanation: "The provider acknowledged the write.",
  },
  responding: {
    label: "responding",
    tone: "dc-info",
    explanation: "The runner is producing output for this message.",
  },
  completed: {
    label: "completed",
    tone: "dc-ok",
    explanation: "The turn this message belongs to finished.",
  },
  interrupted: {
    label: "interrupted",
    tone: "dc-warn",
    explanation: "The turn was interrupted before it finished.",
  },
  rejected: {
    label: "rejected",
    tone: "dc-err",
    explanation: "The hub refused this message. Nothing was delivered.",
  },
  failed: {
    label: "failed",
    tone: "dc-err",
    explanation: "The message did not reach the hub. Nothing was delivered.",
  },
  unknown: {
    label: "unknown",
    tone: "dc-warn",
    explanation:
      "We could not confirm the runner received this. Retry re-queues this message once; nothing is duplicated.",
  },
};

/**
 * `queued` on an unlinked chat is the ordinary path, not a degraded one: the
 * hub holds the message until a runner claims the coordinator work item
 * (decisions.md §9.1). Saying "waiting for a bound runner" in warning tone
 * there would read as a fault, so the chat surface states the fact plainly.
 */
const CHAT_COPY: Record<string, Copy> = {
  queued: {
    label: "Queued for a runner",
    tone: "",
    explanation: "The hub is holding this until a runner picks the chat up. Nothing has failed.",
  },
};

/**
 * Error codes that say more than the ladder step they arrived on. The
 * transitional hub-side coordinator reports `coordinator_unavailable` when the
 * project has no coordinator at all (decisions.md §1); the fix is enrolling a
 * runner, so the chip says so rather than reporting a bare rejection.
 */
const ERROR_COPY: Record<string, Copy> = {
  coordinator_unavailable: {
    label: "No coordinator is available for this project",
    tone: "dc-err",
    explanation: "Enrol a runner for this project so it can answer chats.",
  },
};

const TONE_FILL: Record<string, string> = {
  "dc-ok": "bg-success",
  "dc-warn": "bg-warning",
  "dc-err": "bg-error",
  "dc-info": "bg-info",
  "": "bg-muted-foreground/60",
};

export function DeliveryChip({
  status,
  detail,
  errorCode,
  surface,
}: {
  status: ChipStatus;
  detail?: string | null;
  /** The receipt's error code, where one settled this message. */
  errorCode?: string | null;
  /** `chat` is an unlinked conversation answered by a coordinator turn. */
  surface?: ExecutionSurface;
}): React.ReactElement {
  const copy =
    (errorCode == null ? undefined : ERROR_COPY[errorCode]) ??
    (surface === "chat" ? CHAT_COPY[status] : undefined) ??
    COPY[status] ?? {
      label: status,
      tone: "",
      explanation: "",
    };
  const spoken = [`Delivery: ${copy.label}`, copy.explanation, detail ?? ""]
    .filter((part) => part.length > 0)
    .join(". ");
  return (
    // The explanation is announced through the label rather than a duplicate
    // sr-only node: the same sentence can already be on screen next to the
    // chip, and hearing it twice is worse than not hearing it at all.
    <span
      className="dc-delivery-chip inline-flex min-w-0 items-center gap-1.5 text-muted-foreground text-xs"
      title={copy.explanation}
      aria-label={spoken}
    >
      <span
        className={cn("dc-dot size-1.5 shrink-0 rounded-full", copy.tone, TONE_FILL[copy.tone])}
        aria-hidden="true"
      />
      <span data-testid="delivery-label">{copy.label}</span>
      {detail != null && detail.length > 0 ? (
        <span className="min-w-0 truncate text-muted-foreground/70"> · {detail}</span>
      ) : null}
    </span>
  );
}
