import React from "react";

import { makeAccountApi } from "../../account/api.ts";
import { ClientContext } from "../../client.ts";

export interface RunnerName {
  readonly display: string;
  readonly host: string;
}

export type RunnerNames = ReadonlyMap<string, RunnerName>;

export const NO_RUNNER_NAMES: RunnerNames = new Map();

/**
 * A runner or machine id short enough to read: the prefix and the first
 * eight hex characters of a long generated id. An id that is not of that
 * shape is already a name and is kept whole.
 */
export function shortRunnerId(id: string): string {
  const match = /^([a-z]+)_([0-9a-f]{16,})$/.exec(id);
  if (match === null) return id;
  return `${match[1]}_${match[2]!.slice(0, 8)}`;
}

/** The name to show for a runner id: its display name, else its short id. */
export function runnerDisplay(
  names: RunnerNames | undefined,
  id: string | null | undefined,
): string | null {
  if (id === null || id === undefined || id.length === 0) return null;
  return names?.get(id)?.display ?? shortRunnerId(id);
}

const FLEET_TTL_MS = 60_000;
let cached: { readonly at: number; readonly names: RunnerNames } | null = null;
let inflight: Promise<RunnerNames> | null = null;

/**
 * The fleet's runner names, read at most once a minute per session and shared
 * by every surface that shows an actor. A fleet that cannot be read yields no
 * names rather than an error: the ids still identify the runner.
 */
export function useRunnerNames(): RunnerNames {
  // Read the client directly rather than through `useClient`, which throws
  // where no client is mounted: a surface drawn on its own (tests, previews)
  // shows short ids instead of failing.
  const client = React.useContext(ClientContext);
  const [names, setNames] = React.useState<RunnerNames>(() => cached?.names ?? NO_RUNNER_NAMES);
  React.useEffect(() => {
    if (client === null) return;
    let cancelled = false;
    const refresh = () => {
      if (cached !== null && Date.now() - cached.at < FLEET_TTL_MS) {
        setNames(cached.names);
        return;
      }
      inflight ??= makeAccountApi({
        origin: client.http.origin,
        apiBase: client.http.apiBase,
        csrfToken: client.http.csrfToken,
      })
        .fleet()
        .then((fleet) => {
          const next = new Map<string, RunnerName>();
          for (const [id, runner] of Object.entries(fleet.runner_names ?? {})) {
            next.set(id, { display: runner.display_name, host: runner.hostname });
          }
          for (const runner of fleet.runners) {
            next.set(runner.id, { display: runner.display_name, host: runner.hostname });
          }
          cached = { at: Date.now(), names: next };
          return next as RunnerNames;
        })
        .catch(() => cached?.names ?? NO_RUNNER_NAMES)
        .finally(() => {
          inflight = null;
        });
      void inflight.then((next) => {
        if (!cancelled) setNames(next);
      });
    };
    refresh();
    // A surface that stays open outlives the cache: read the fleet again
    // each time the entry expires, so a renamed or newly enrolled runner
    // gets its name without a reload.
    const timer = setInterval(refresh, FLEET_TTL_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [client]);
  return names;
}

/** For tests: forget the cached fleet. */
export function resetRunnerNamesForTests(): void {
  cached = null;
  inflight = null;
}
