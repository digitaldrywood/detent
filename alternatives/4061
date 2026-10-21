import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import type { SuggestedLimit } from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";

function shownSuggestion(suggestion: SuggestedLimit | undefined): SuggestedLimit | undefined {
  if (suggestion?.direction === "insufficient_evidence") return suggestion;
  return (suggestion?.direction === "lower" || suggestion?.direction === "raise") && suggestion.limit !== undefined ? suggestion : undefined;
}

export function SuggestedLimitChip({ suggestion }: { readonly suggestion: SuggestedLimit | undefined }): React.ReactElement | null {
  const shown = shownSuggestion(suggestion);
  if (shown === undefined) return null;
  if (shown.direction === "insufficient_evidence") return <Badge size="sm" variant="outline" className="text-muted-foreground" data-testid="suggested-limit">Gathering data</Badge>;
  if (shown.direction === "lower") return <Badge size="sm" variant="warning" data-testid="suggested-limit">Suggest {shown.limit} ↓</Badge>;
  return <Badge size="sm" variant="info" data-testid="suggested-limit">Room for {shown.limit} ↑</Badge>;
}

export function SuggestedLimitBox({ suggestion, draftLimit, onUse }: {
  readonly suggestion: SuggestedLimit;
  readonly draftLimit: number;
  readonly onUse?: (limit: number) => void;
}): React.ReactElement | null {
  const shown = shownSuggestion(suggestion);
  if (shown === undefined) return null;
  const evidence = shown.direction === "insufficient_evidence" || shown.limit === undefined;
  const offer = !evidence && onUse !== undefined && shown.limit !== draftLimit;
  return (
    <div data-testid="suggested-limit-box" className={cn("space-y-2 rounded-lg border p-3", evidence ? "border-border/60" : shown.direction === "lower" ? "border-warning/30 bg-warning/8" : "border-border/60 bg-muted/40")}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="font-medium">{evidence ? "Not enough data yet" : `Suggested: ${shown.limit} ${shown.limit === 1 ? "job" : "jobs"} at once`}</p>
        {offer ? <Button type="button" size="sm" onClick={() => onUse(shown.limit!)}>Use {shown.limit}</Button> : null}
      </div>
      {shown.reasons.length > 0 ? <ul className="list-disc space-y-0.5 pl-4 text-xs">{shown.reasons.map((reason) => <li key={reason}>{reason}</li>)}</ul> : null}
      <p className="text-xs text-muted-foreground">Based on {shown.hours_covered} covered hours.{offer ? ` "Use ${shown.limit}" fills Jobs at once. Save runner applies it.` : ""}</p>
    </div>
  );
}
