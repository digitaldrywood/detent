import { RegistryContext } from "@effect/atom-react";
import * as Cause from "effect/Cause";
import { AsyncResult, type Atom } from "effect/unstable/reactivity";
import { useCallback, useContext } from "react";

import type { AtomCommandResult } from "../runtime/state/runtime.ts";

/** Upstream's options bag. Detent has no failure reporter, so these are read and ignored. */
export interface AtomQueryOptions {
  readonly label?: string;
  readonly reportFailure?: boolean;
  readonly reportDefect?: boolean;
  readonly refresh?: boolean;
}

export function useAtomQueryRunner<T, A, E>(
  family: (target: T) => Atom.Atom<AsyncResult.AsyncResult<A, E>>,
  _options?: string | AtomQueryOptions,
): (target: T) => Promise<AtomCommandResult<A, E>> {
  const registry = useContext(RegistryContext);
  return useCallback(
    async (target: T): Promise<AtomCommandResult<A, E>> => {
      const result = registry.get(family(target));
      // A constant family settles immediately. `initial`/`waiting` cannot
      // occur here, but the copied callers only branch on `_tag`, so an
      // unsettled value is reported as the failure they already handle.
      if (result._tag === "Success" || result._tag === "Failure") {
        return result;
      }
      return AsyncResult.failure<A, E>(
        Cause.die(new Error("This request has no source in Detent Cloud.")),
      );
    },
    [family, registry],
  );
}
