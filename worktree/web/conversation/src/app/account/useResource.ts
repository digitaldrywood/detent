// One async read, with the three states every account screen renders.
//
// There is no query library here on purpose: these screens read once, refresh
// after a mutation they made, and have no stream behind them. What matters is
// that a response for a request the screen no longer wants is dropped rather
// than painted, and that a failure arrives as the typed `AccountError` the
// screens map to a friendly state.
import React from "react";

import { AccountError } from "./api.ts";

export interface Resource<A> {
  readonly value: A | undefined;
  readonly error: AccountError | null;
  readonly loading: boolean;
  /** Re-runs the read. Awaited by callers that act on the fresh value. */
  readonly refresh: () => Promise<void>;
  /** Replaces the value without a round trip, for a mutation's own response. */
  readonly set: (value: A) => void;
}

export function useResource<A>(
  read: () => Promise<A>,
  dependencies: readonly unknown[],
): Resource<A> {
  const [value, setValue] = React.useState<A | undefined>(undefined);
  const [error, setError] = React.useState<AccountError | null>(null);
  const [loading, setLoading] = React.useState(true);
  // A generation counter rather than an AbortController: the responses are
  // small and already in flight, and all that matters is that a stale one
  // never becomes the rendered value.
  const generation = React.useRef(0);
  const latest = React.useRef(read);
  latest.current = read;

  const run = React.useCallback(async () => {
    const mine = generation.current + 1;
    generation.current = mine;
    setLoading(true);
    try {
      const next = await latest.current();
      if (generation.current !== mine) return;
      setValue(next);
      setError(null);
    } catch (cause) {
      if (generation.current !== mine) return;
      setError(
        cause instanceof AccountError
          ? cause
          : new AccountError({
              status: 0,
              code: "invalid",
              message: cause instanceof Error ? cause.message : String(cause),
            }),
      );
    } finally {
      if (generation.current === mine) setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void run();
    return () => {
      // Anything still in flight belongs to a screen that is going away.
      generation.current += 1;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, dependencies);

  return { value, error, loading, refresh: run, set: setValue };
}

/**
 * A mutation with the one piece of state its button needs: whether it is in
 * flight, and what went wrong. Errors are never thrown at the render tree —
 * a refused mutation is a message next to the control that asked for it.
 */
export function useMutation<Args extends readonly unknown[], A>(
  run: (...args: Args) => Promise<A>,
): {
  readonly pending: boolean;
  readonly error: AccountError | null;
  readonly clearError: () => void;
  readonly call: (...args: Args) => Promise<A | null>;
} {
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<AccountError | null>(null);
  const latest = React.useRef(run);
  latest.current = run;

  const call = React.useCallback(async (...args: Args): Promise<A | null> => {
    setPending(true);
    setError(null);
    try {
      return await latest.current(...args);
    } catch (cause) {
      setError(
        cause instanceof AccountError
          ? cause
          : new AccountError({
              status: 0,
              code: "invalid",
              message: cause instanceof Error ? cause.message : String(cause),
            }),
      );
      return null;
    } finally {
      setPending(false);
    }
  }, []);

  return { pending, error, clearError: React.useCallback(() => setError(null), []), call };
}
