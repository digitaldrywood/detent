// Live conversation handles.
//
// Written for this repository. An open conversation's state lives inside the
// scope of its atom, but sending a command has to touch that state (optimistic
// message in, receipt applied) from a component that only has an id. This
// registry is the seam: one entry per open conversation, added when the
// subscription starts and removed by its scope finalizer, so "one detail
// subscription per open conversation, shared across components" also means one
// place to write to.
//
// The registry belongs to a client, not to the module. Two clients in one
// process (a test standing in for a second browser tab) open the same
// conversation id, and a shared map would let one client's command land in the
// other client's state.
import * as Effect from "effect/Effect";

import type { ConversationDetail } from "./conversationState.ts";

export interface ConversationHandle {
  readonly loadOlder: Effect.Effect<void>;
  readonly refresh: Effect.Effect<void>;
  readonly setDetail: (
    update: (detail: ConversationDetail) => ConversationDetail,
  ) => Effect.Effect<void>;
}

export class ConversationHandles {
  private readonly handles = new Map<string, ConversationHandle>();

  /** Registers a handle for the lifetime of the calling scope. */
  register(key: string, handle: ConversationHandle) {
    return Effect.acquireRelease(
      Effect.sync(() => {
        this.handles.set(key, handle);
      }),
      () =>
        Effect.sync(() => {
          if (this.handles.get(key) === handle) this.handles.delete(key);
        }),
    );
  }

  get(key: string): ConversationHandle | undefined {
    return this.handles.get(key);
  }

  /** Runs an update against an open conversation; a closed one is a no-op. */
  update(
    key: string,
    update: (detail: ConversationDetail) => ConversationDetail,
  ): Effect.Effect<void> {
    const handle = this.handles.get(key);
    return handle === undefined ? Effect.void : handle.setDetail(update);
  }

  /** Test seam: forget every handle. */
  clear(): void {
    this.handles.clear();
  }
}
