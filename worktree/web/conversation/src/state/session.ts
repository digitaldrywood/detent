import * as Option from "effect/Option";
import { useMemo } from "react";

export interface PreparedConnection {
  readonly httpBaseUrl: string;
}

export function usePreparedConnection(
  environmentId: string | null,
): Option.Option<PreparedConnection> {
  return useMemo(() => {
    const connection = readPreparedConnection(environmentId);
    return connection === null ? Option.none() : Option.some(connection);
  }, [environmentId]);
}

export function readPreparedConnection(environmentId: string | null): PreparedConnection | null {
  if (environmentId === null) return null;
  const origin = globalThis.location?.origin;
  if (origin === undefined || origin === "null") return null;
  return { httpBaseUrl: origin };
}
