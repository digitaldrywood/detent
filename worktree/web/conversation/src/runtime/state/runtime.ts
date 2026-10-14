import * as Cause from "effect/Cause";
import * as Effect from "effect/Effect";
import * as Stream from "effect/Stream";
import { AsyncResult } from "effect/unstable/reactivity";
import { EnvironmentRegistry } from "../connection/registry.ts";
import type { EnvironmentSupervisor } from "../connection/supervisor.ts";
export function followStreamInEnvironment<A, E, R>(
  environmentId: string,
  stream: Stream.Stream<A, E, R>,
): Stream.Stream<
  A,
  E,
  EnvironmentRegistry | Exclude<R, EnvironmentSupervisor>
> {
  return Stream.unwrap(
    Effect.map(EnvironmentRegistry, (registry) =>
      registry.followStream(environmentId, stream),
    ),
  );
}

export type SettledAsyncResult<A, E> = AsyncResult.Success<A, E> | AsyncResult.Failure<A, E>;

export type AtomCommandResult<A, E> = SettledAsyncResult<A, E>;

export type AtomCommandFailure<R> = R extends AtomCommandResult<infer _A, infer E> ? E : never;

export function mapAtomCommandResult<A, E, B>(
  result: AtomCommandResult<A, E>,
  map: (value: A) => B,
): AtomCommandResult<B, E> {
  return result._tag === "Success"
    ? AsyncResult.success(map(result.value))
    : AsyncResult.failure(result.cause);
}

export function isAtomCommandInterrupted(result: AtomCommandResult<unknown, unknown>): boolean {
  return result._tag === "Failure" && Cause.hasInterruptsOnly(result.cause);
}

export function squashAtomCommandFailure(result: {
  readonly cause: Cause.Cause<unknown>;
}): unknown {
  return Cause.squash(result.cause);
}

export async function settlePromise<A>(
  execute: () => Promise<A>,
): Promise<AtomCommandResult<A, never>> {
  try {
    return AsyncResult.success(await execute());
  } catch (defect) {
    return AsyncResult.failure(Cause.die(defect));
  }
}
