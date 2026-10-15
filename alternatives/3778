import { CircleAlertIcon } from "lucide-react";

import { Button } from "../../components/ui/button.tsx";
import { ComposerBanner } from "../../components/chat/ComposerBanner.tsx";
import type {
  ComposerBannerStackContent,
  ComposerBannerStackItem,
} from "../../components/chat/ComposerBannerStack.tsx";
import { feedbackBannerItem } from "../../components/chat/ComposerFeedback.tsx";
import { usageLimitsBannerItem } from "../../components/chat/ComposerUsageLimits.tsx";
import type { UsageLimitsReport } from "../../contracts/index.ts";
import type { CodexFeedbackSubmission } from "../../state/threads.ts";
import type { ComposerBannerEntry } from "../components/Composer.tsx";
import { ExecutionStrip, type ExecutionStripProps } from "../components/ExecutionStrip.tsx";

export function executionBannerItem(props: ExecutionStripProps): ComposerBannerStackContent {
  return {
    id: "execution",
    variant: "info",
    priority: "activity",

    content: (
      <ComposerBanner.Body>
        <ExecutionStrip {...props} />
      </ComposerBanner.Body>
    ),
  };
}

export function threadErrorBannerItem(
  message: string,
  onDismiss: () => void,
  onRetry?: () => void,
): ComposerBannerStackItem {
  return {
    id: "thread-error",
    variant: "error",
    priority: "urgent",
    icon: <CircleAlertIcon />,
    title: "This conversation reported an error",
    description: message,
    dismissLabel: "Dismiss the error",
    onDismiss,
    ...(onRetry === undefined
      ? {}
      : {
          actions: (
            <Button size="xs" variant="ghost" onClick={onRetry}>
              Retry
            </Button>
          ),
        }),
  };
}

export interface ComposerBannerInput {
  readonly execution?: ExecutionStripProps | undefined;
  readonly threadError?:
    | { readonly message: string; readonly onDismiss: () => void; readonly onRetry?: () => void }
    | undefined;
  /** The entitlement allowances, when the hub reported any (§17.5). */
  readonly usageLimits?:
    | { readonly report: UsageLimitsReport; readonly onDismiss: () => void }
    | undefined;
  /** Never non-empty today; see the header. */
  readonly feedback?:
    | { readonly submission: CodexFeedbackSubmission; readonly onDismiss: () => void }
    | undefined;
}

/**
 * The rail. Order here does not matter — `ComposerBannerStack` sorts by its own
 * priority — so this reads in the order the pieces are described above.
 */
export function composerBanners(input: ComposerBannerInput): ComposerBannerEntry[] {
  const items: ComposerBannerEntry[] = [];
  if (input.execution !== undefined) items.push(executionBannerItem(input.execution));
  if (input.threadError !== undefined) {
    items.push(
      threadErrorBannerItem(
        input.threadError.message,
        input.threadError.onDismiss,
        input.threadError.onRetry,
      ),
    );
  }
  if (input.usageLimits !== undefined) {
    items.push(
      usageLimitsBannerItem(
        "usage-limits",
        input.usageLimits.report,
        "hub",
        input.usageLimits.onDismiss,
      ),
    );
  }
  if (input.feedback !== undefined) {
    const item = feedbackBannerItem(input.feedback.submission, input.feedback.onDismiss);
    if (item !== null) items.push(item);
  }
  return items;
}

/** Re-exported so a caller can hold the type without reaching into `components/`. */
export type { ComposerBannerStackItem, ComposerBannerStackContent };
