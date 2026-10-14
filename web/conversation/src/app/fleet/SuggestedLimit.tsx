import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import type { SuggestedLimit } from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";

export function SuggestedLimitChip({ suggestion }: { readonly suggestion: SuggestedLimit | undefined }): React.ReactElement | null {
  if (suggestion?.direction === "insufficient_evidence") return <Badge size="sm" variant="outline" className="text-muted-foreground" data-testid="suggested-limit">Gathering data</Badge>;
  if (suggestion?.limit === undefined) return null;
  if (suggestion.direction === "lower") return <Badge size="sm" variant="warning" data-testid="suggested-limit">Suggest {suggestion.limit} ↓</Badge>;
  if (suggestion.direction === "raise") return <Badge size="sm" variant="info" data-testid="suggested-limit">Room for {suggestion.limit} ↑</Badge>;
  return null;
}

export function SuggestedLimitBox({ suggestion, onUse }: {
  readonly suggestion: SuggestedLimit;
  readonly onUse?: (limit: number) => void;
}): React.ReactElement {
  const evidence = suggestion.direction === "insufficient_evidence" || suggestion.limit === undefined;
  return (
    <div data-testid="suggested-limit-box" className={cn("space-y-2 rounded-lg border p-3", evidence ? "border-border/60" : suggestion.direction === "lower" ? "border-warning/30 bg-warning/8" : "border-border/60 bg-muted/40")}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="font-medium">{evidence ? "Not enough data yet" : `Suggested: ${suggestion.limit} ${suggestion.limit === 1 ? "job" : "jobs"} at once`}</p>
        {!evidence && suggestion.limit !== suggestion.current && onUse ? <Button type="button" size="sm" onClick={() => onUse(suggestion.limit!)}>Use {suggestion.limit}</Button> : null}
      </div>
      {suggestion.reasons.length > 0 ? <ul className="list-disc space-y-0.5 pl-4 text-xs">{suggestion.reasons.map((reason) => <li key={reason}>{reason}</li>)}</ul> : null}
      <p className="text-xs text-muted-foreground">Based on {suggestion.hours_covered} covered hours.{!evidence && onUse && suggestion.limit !== suggestion.current ? ` "Use ${suggestion.limit}" fills Jobs at once. Save runner applies it.` : ""}</p>
    </div>
  );
}
