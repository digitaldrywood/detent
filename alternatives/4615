import * as Context from "effect/Context";
import * as Effect from "effect/Effect";
import * as Option from "effect/Option";
import * as Schema from "effect/Schema";

import type { Conversation, ConversationSnapshot } from "../../contracts/index.ts";
import type { ConnectionRegistration } from "../connection/catalog.ts";
import type { ConnectionTarget } from "../connection/model.ts";

export class ConnectionPersistenceError extends Schema.TaggedError<ConnectionPersistenceError>()(
  "ConnectionPersistenceError",
  { message: Schema.String },
) {}

export class ConnectionTargetStore extends Context.Service<
  ConnectionTargetStore,
  {
    list: Effect.Effect<ReadonlyArray<ConnectionTarget>, ConnectionPersistenceError>;
  }
>()("ConnectionTargetStore") {}

export class ConnectionRegistrationStore extends Context.Service<
  ConnectionRegistrationStore,
  {
    register: (r: ConnectionRegistration) => Effect.Effect<void, ConnectionPersistenceError>;
    remove: (t: ConnectionTarget) => Effect.Effect<void, ConnectionPersistenceError>;
  }
>()("ConnectionRegistrationStore") {}

export class EnvironmentOwnedDataCleanup extends Context.Service<
  EnvironmentOwnedDataCleanup,
  { clear: (id: string) => Effect.Effect<void> }
>()("EnvironmentOwnedDataCleanup") {}

export class EnvironmentCacheStore extends Context.Service<
  EnvironmentCacheStore,
  {
    loadShell: (
      id: string,
    ) => Effect.Effect<Option.Option<ReadonlyArray<Conversation>>, ConnectionPersistenceError>;
    saveShell: (
      id: string,
      conversations: ReadonlyArray<Conversation>,
    ) => Effect.Effect<void, ConnectionPersistenceError>;
    loadThread: (
      id: string,
      conversationId: string,
    ) => Effect.Effect<Option.Option<ConversationSnapshot>, ConnectionPersistenceError>;
    saveThread: (
      id: string,
      snapshot: ConversationSnapshot,
    ) => Effect.Effect<void, ConnectionPersistenceError>;
    removeThread: (
      id: string,
      conversationId: string,
    ) => Effect.Effect<void, ConnectionPersistenceError>;
    clear: (id: string) => Effect.Effect<void, ConnectionPersistenceError>;
  }
>()("EnvironmentCacheStore") {}

/**
 * Session-lifetime cache. Cleared on logout or account change (rule 8 in
 * decisions.md §3), which for an in-memory map means the page reload that
 * follows a sign-out already clears it; `clear` covers the same-tab case.
 */
export function memoryCache(): EnvironmentCacheStore["Service"] {
  const shells = new Map<string, ReadonlyArray<Conversation>>();
  const conversations = new Map<string, ConversationSnapshot>();
  const key = (id: string, conversationId: string) => JSON.stringify([id, conversationId]);
  return {
    loadShell: (id) => Effect.sync(() => Option.fromUndefinedOr(shells.get(id))),
    saveShell: (id, value) =>
      Effect.sync(() => {
        shells.set(id, value);
      }),
    loadThread: (id, conversationId) =>
      Effect.sync(() => Option.fromUndefinedOr(conversations.get(key(id, conversationId)))),
    saveThread: (id, snapshot) =>
      Effect.sync(() => {
        conversations.set(key(id, snapshot.conversation.id), snapshot);
      }),
    removeThread: (id, conversationId) =>
      Effect.sync(() => {
        conversations.delete(key(id, conversationId));
      }),
    clear: (id) =>
      Effect.sync(() => {
        shells.delete(id);
        for (const stored of [...conversations.keys()]) {
          if (JSON.parse(stored)[0] === id) conversations.delete(stored);
        }
      }),
  };
}
