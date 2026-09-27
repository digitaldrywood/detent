import * as Effect from "effect/Effect";
import * as Option from "effect/Option";
import { AsyncResult, AtomRegistry, type Atom } from "effect/unstable/reactivity";

import {
  startMockHub,
  type AccountMode,
  type CoordinatorMode,
  type MockHub,
} from "../dev/mock-hub.ts";
import { loadBootstrap, makeClient, type ConversationClient } from "../src/runtime/bootstrap.ts";
import { HUB_ENVIRONMENT_ID } from "../src/contracts/index.ts";
import { fetchEventStreamTransport } from "../src/runtime/rpc/sse.ts";

export interface Harness {
  readonly hub: MockHub;
  readonly client: ConversationClient;
  readonly registry: AtomRegistry.AtomRegistry;
  readonly mounted: Array<() => void>;
  readonly run: <A>(effect: Effect.Effect<A>) => Promise<A>;
  readonly mount: <A>(atom: Atom.Atom<A>) => void;
  readonly read: <A, E>(atom: Atom.Atom<AsyncResult.AsyncResult<A, E>>) => A | undefined;
  readonly waitFor: <A, E>(
    atom: Atom.Atom<AsyncResult.AsyncResult<A, E>>,
    predicate: (value: A) => boolean,
    label?: string,
  ) => Promise<A>;
  readonly control: (path: string, body?: unknown) => Promise<void>;
  readonly dispose: () => Promise<void>;
}

export interface HarnessOptions {
  /**
   * An existing hub to attach to instead of starting one. A harness that did
   * not start the hub never closes it, so two clients can share it and be
   * disposed independently (the second-tab tests).
   */
  readonly hub?: MockHub;
  /** Which coordinator the hub runs. Default `hub`; `runner` is §9's path. */
  readonly coordinator?: CoordinatorMode;
  /** Whether the signed-in account can write. Default `write` (§10.11). */
  readonly account?: AccountMode;
  /**
   * Replaces the HTTP fetch. The event stream keeps its own, so a test can
   * delay or fail a command without touching the stream that races it.
   */
  readonly fetch?: typeof globalThis.fetch;
}

export async function makeHarness(options: HarnessOptions = {}): Promise<Harness> {
  const ownsHub = options.hub === undefined;
  const hub =
    options.hub ??
    (await startMockHub({
      deltaDelayMs: 0,
      heartbeatMs: 5_000,
      ...(options.coordinator === undefined ? {} : { coordinator: options.coordinator }),
      ...(options.account === undefined ? {} : { account: options.account }),
    }));
  const bootstrap = await loadBootstrap(hub.url);
  const client = makeClient({
    origin: hub.url,
    bootstrap,
    transport: fetchEventStreamTransport(),
    heartbeatTimeoutMs: 20_000,
    ...(options.fetch === undefined ? {} : { fetch: options.fetch }),
  });
  const registry = AtomRegistry.make();
  const mounted: Array<() => void> = [];

  const read = <A, E>(atom: Atom.Atom<AsyncResult.AsyncResult<A, E>>): A | undefined =>
    Option.getOrUndefined(AsyncResult.value(registry.get(atom)));

  const waitFor = <A, E>(
    atom: Atom.Atom<AsyncResult.AsyncResult<A, E>>,
    predicate: (value: A) => boolean,
    label = "condition",
  ): Promise<A> =>
    new Promise<A>((resolve, reject) => {
      let unsubscribe: (() => void) | undefined;
      let poll: ReturnType<typeof setInterval> | undefined;
      let timer: ReturnType<typeof setTimeout> | undefined;
      let settled = false;
      const settle = () => {
        settled = true;
        if (timer !== undefined) clearTimeout(timer);
        if (poll !== undefined) clearInterval(poll);
        unsubscribe?.();
      };
      const check = () => {
        if (settled) return true;
        const value = read(atom);
        if (value !== undefined && predicate(value)) {
          settle();
          resolve(value);
          return true;
        }
        return false;
      };
      unsubscribe = registry.subscribe(atom, () => {
        check();
      });
      timer = setTimeout(() => {
        settle();
        reject(new Error(`Timed out waiting for ${label}.`));
      }, 15_000);
      if (!check()) {
        // Some transitions land between the read and the subscription.
        poll = setInterval(check, 25);
      }
    });

  return {
    hub,
    client,
    registry,
    mounted,
    run: (effect) => Effect.runPromise(effect),
    mount: (atom) => {
      mounted.push(registry.mount(atom));
    },
    read,
    waitFor,
    control: async (path, body) => {
      await fetch(`${hub.url}/__mock/${path}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body ?? {}),
      });
    },
    dispose: async () => {
      for (const unmount of mounted) unmount();
      registry.dispose();
      client.handles.clear();
      if (ownsHub) await hub.close();
    },
  };
}

export const ENVIRONMENT = HUB_ENVIRONMENT_ID;
export const PROJECT = "proj_alpha";
