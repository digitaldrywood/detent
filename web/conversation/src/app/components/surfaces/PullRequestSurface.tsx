import { ExternalLinkIcon } from "lucide-react";
import type { ReactElement } from "react";

import { resolvePullRequestState } from "../../../components/pullRequest/pullRequestPresentation.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { cn } from "../../../lib/utils.ts";
import type { PullRequestSurfaceData } from "../../adapters/surfaces.ts";

export function PullRequestSurface({
  pullRequest,
}: {
  readonly pullRequest: PullRequestSurfaceData | null;
}): ReactElement {
  if (pullRequest === null) {
    return (
      <div
        className="flex h-full items-center justify-center px-3 py-2 text-center text-muted-foreground/70 text-xs"
        data-testid="pull-request-empty"
      >
        <p>No pull request on this branch yet.</p>
      </div>
    );
  }
  const presentation = resolvePullRequestState({
    state: pullRequest.state,
    isDraft: pullRequest.isDraft,
  });
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3 p-3" data-testid="pull-request-surface">
      <div className="flex items-center gap-2">
        <presentation.Icon className={cn("size-4 shrink-0", presentation.toneClassName)} />
        <span className={cn("font-medium text-sm", presentation.toneClassName)}>
          {presentation.label}
        </span>
        <span className="ml-auto font-mono text-muted-foreground text-xs tabular-nums">
          #{pullRequest.number}
        </span>
      </div>
      <p className="text-balance font-medium text-sm leading-snug">{pullRequest.title}</p>
      <p className="font-mono text-[11px] text-muted-foreground">{pullRequest.repository}</p>
      <Button
        size="sm"
        variant="outline"
        className="w-fit"
        data-testid="pull-request-open"
        onClick={() => globalThis.open?.(pullRequest.url, "_blank", "noopener")}
      >
        <ExternalLinkIcon className="size-3.5" />
        Open on the host
      </Button>
    </div>
  );
}
