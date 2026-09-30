import React from "react";

import { DiffPanelLoadingState } from "../../../components/DiffPanelShell.tsx";
import { Button } from "../../../components/ui/button.tsx";
import {
  formatElapsed,
  type WorkspaceStatusPresentation,
} from "../../lib/workspaceStatus.ts";

/** The elapsed clock under a wait, re-read every second while it is shown. */
export function useElapsedLabel(since: string | null): string | null {
  const started = React.useMemo(() => {
    if (since === null || since.length === 0) return null;
    const parsed = Date.parse(since);
    return Number.isFinite(parsed) ? parsed : null;
  }, [since]);
  const [now, setNow] = React.useState(() => Date.now());
  React.useEffect(() => {
    if (started === null) return;
    setNow(Date.now());
    const timer = globalThis.setInterval(() => setNow(Date.now()), 1000);
    return () => globalThis.clearInterval(timer);
  }, [started]);
  if (started === null) return null;
  return formatElapsed(now - started);
}

export function WorkspaceStatusView({
  status,
  testIdPrefix,
  onRetry,
}: {
  readonly status: WorkspaceStatusPresentation;
  /** `files` or `output`; the surface owns its own test ids. */
  readonly testIdPrefix: string;
  readonly onRetry: () => void;
}): React.ReactElement {
  const elapsed = useElapsedLabel(status.elapsedSince);
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-auto">
      <div
        className="flex flex-col items-center justify-center gap-2 px-3 py-6 text-center text-muted-foreground/70 text-xs"
        data-testid={`${testIdPrefix}-${status.testId}`}
        data-workspace-status={status.kind}
        role="status"
        aria-live="polite"
      >
        <p>{status.sentence}</p>
        {elapsed !== null ? (
          <p className="tabular-nums" data-testid={`${testIdPrefix}-elapsed`}>
            {elapsed} elapsed
          </p>
        ) : null}
        {status.retryLabel !== null ? (
          <Button size="xs" variant="outline" onClick={onRetry}>
            {status.retryLabel}
          </Button>
        ) : null}
      </div>
      {status.skeleton ? (
        <div className="flex min-h-0 flex-1 flex-col" aria-hidden="true">
          <DiffPanelLoadingState label={status.sentence} />
        </div>
      ) : null}
    </div>
  );
}
