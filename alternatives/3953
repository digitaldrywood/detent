import React from "react";

import type { RunnerCapacityRead, SuggestedLimit } from "../../contracts/account.ts";

export interface RunnerRevision {
  readonly id: string;
  readonly revision: number;
}

export function useRunnerSuggestions(
  read: (runner: string) => Promise<RunnerCapacityRead>,
  runners: readonly RunnerRevision[],
  enabled: boolean,
): Readonly<Record<string, SuggestedLimit>> {
  const [suggestions, setSuggestions] = React.useState<Readonly<Record<string, SuggestedLimit>>>({});
  const requested = React.useRef(new Map<string, number>());
  React.useEffect(() => {
    if (!enabled) return;
    const forget = (id: string) => setSuggestions((current) => {
      if (!(id in current)) return current;
      const next = { ...current };
      delete next[id];
      return next;
    });
    const live = new Set(runners.map((runner) => runner.id));
    for (const id of [...requested.current.keys()]) {
      if (!live.has(id)) {
        requested.current.delete(id);
        forget(id);
      }
    }
    for (const { id, revision } of runners) {
      if (requested.current.get(id) === revision) continue;
      requested.current.set(id, revision);
      forget(id);
      read(id).then((capacity) => {
        const suggestion = capacity.recommendation?.suggested_limit;
        if (requested.current.get(id) !== revision || suggestion === undefined) return;
        setSuggestions((current) => ({ ...current, [id]: suggestion }));
      }, () => undefined);
    }
  }, [read, runners, enabled]);
  return suggestions;
}
