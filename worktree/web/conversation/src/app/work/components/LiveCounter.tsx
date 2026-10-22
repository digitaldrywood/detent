import React from "react";

import { Tooltip, TooltipPopup, TooltipTrigger } from "../../../components/ui/tooltip.tsx";
import { cn } from "../../../lib/utils.ts";

export function LiveCounter({ count, loading }: { count: number | null; loading: boolean }): React.ReactElement {
  const live = count !== null && count > 0;
  const label = `${count ?? "—"} live`;
  return (
    <div aria-busy={loading} className={cn("flex shrink-0 items-center gap-1 text-muted-foreground text-xs", loading && "opacity-50")}>
      <Tooltip>
        <TooltipTrigger
          render={<span role="status" tabIndex={0} aria-label={label} data-testid="stat-live" className="inline-flex min-h-6 items-center gap-1.5 whitespace-nowrap rounded-md px-1.5" />}
        >
          <span
            aria-hidden
            className={cn(
              "size-1.5 shrink-0 rounded-full",
              live ? "bg-sky-500 motion-safe:animate-status-pulse dark:bg-sky-300/80" : "bg-muted-foreground/50",
            )}
          />
          <span>
            <b className="font-semibold text-foreground tabular-nums">{count ?? "—"}</b> live
          </span>
        </TooltipTrigger>
        <TooltipPopup side="bottom" className="max-w-72 whitespace-normal leading-tight">
          Items in non-terminal lanes with a running worker, across the current filter scope.
        </TooltipPopup>
      </Tooltip>
      <span aria-hidden className="text-muted-foreground/60">·</span>
    </div>
  );
}
