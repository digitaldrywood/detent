import * as Effect from "effect/Effect";
import * as Option from "effect/Option";
import * as Queue from "effect/Queue";
import * as Stream from "effect/Stream";
import * as SubscriptionRef from "effect/SubscriptionRef";
import { Atom } from "effect/unstable/reactivity";

import type { Conversation } from "../../contracts/index.ts";
import { connectionProjectionPhase } from "../connection/model.ts";
import { EnvironmentRegistry } from "../connection/registry.ts";
import { EnvironmentSupervisor } from "../connection/supervisor.ts";
import { EnvironmentCacheStore } from "../platform/persistence.ts";
import { followStreamInEnvironment } from "./runtime.ts";

type Listener = (conversation: Conversation) => void;

/** In-process fan-out from open conversation streams to the sidebar list. */
class ConversationBus {
  private readonly listeners = new Set<Listener>();

  publish(conversation: Conversation): void {
    for (const listener of [...this.listeners]) listener(conversation);
  }

  subscribe(listener: Listener): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }
}

export const conversationBus = new ConversationBus();

export interface ConversationListState {
  readonly conversations: readonly Conversation[];
  readonly status: "empty" | "cached" | "synchronizing" | "live";
  readonly error: Option.Option<string>;
  readonly nextCursor: string | null;
}

export const EMPTY_CONVERSATION_LIST_STATE: ConversationListState = {
  conversations: [],
  status: "empty",
  error: Option.none(),
  nextCursor: null,
};

function sortByActivity(conversations: readonly Conversation[]): readonly Conversation[] {
  const at = (conversation: Conversation) => {
    const parsed = Date.parse(conversation.last_message_at ?? conversation.updated_at);
    return Number.isNaN(parsed) ? 0 : parsed;
  };
  return conversations
    .slice()
    .toSorted((left, right) => at(right) - at(left) || left.id.localeCompare(right.id));
}

export function mergeConversation(
  conversations: readonly Conversation[],
  incoming: Conversation,
): readonly Conversation[] {
  const index = conversations.findIndex((candidate) => candidate.id === incoming.id);
  if (index < 0) return sortByActivity([...conversations, incoming]);
  const existing = conversations[index];
  // Revisions are per-conversation and monotonic: a late event never wins.
  if (existing !== undefined && existing.revision > incoming.revision) return conversations;
  const next = conversations.slice();
  next[index] = incoming;
  return sortByActivity(next);
}

export const makeConversationListState = Effect.fn("ConversationList.make")(function* () {
  const supervisor = yield* EnvironmentSupervisor;
  const cache = yield* EnvironmentCacheStore;
  const environmentId = supervisor.target.environmentId;

  const cached = yield* cache
    .loadShell(environmentId)
    .pipe(Effect.orElseSucceed(() => Option.none<ReadonlyArray<Conversation>>()));

  const state = yield* SubscriptionRef.make<ConversationListState>({
    ...EMPTY_CONVERSATION_LIST_STATE,
    conversations: Option.getOrElse(cached, () => [] as ReadonlyArray<Conversation>),
    status: Option.isSome(cached) ? "cached" : "empty",
  });

  const refresh = Effect.gen(function* () {
    const session = yield* SubscriptionRef.get(supervisor.session);
    if (Option.isNone(session)) return;
    yield* SubscriptionRef.update(state, (current) => ({
      ...current,
      status: current.conversations.length > 0 ? current.status : "synchronizing",
    }));
    const result = yield* session.value.http
      .listOrganizationConversations({ limit: 100 })
      .pipe(Effect.asSome, Effect.orElseSucceed(() => Option.none()));
    if (Option.isNone(result)) {
      yield* SubscriptionRef.update(state, (current) => ({
        ...current,
        status: current.conversations.length > 0 ? ("cached" as const) : ("empty" as const),
        error: Option.some("The conversation list could not be loaded."),
      }));
      return;
    }
    yield* SubscriptionRef.set(state, {
      conversations: sortByActivity(result.value.conversations),
      status: "live",
      error: Option.none(),
      nextCursor: result.value.next_cursor,
    });
    yield* cache.saveShell(environmentId, result.value.conversations).pipe(Effect.ignore);
  });

  // Events from any open conversation stream keep the sidebar honest without
  // a poll and without an organization-wide stream this milestone does not have.
  const updates = yield* Queue.unbounded<Conversation>();
  const unsubscribe = conversationBus.subscribe((conversation) => {
    Queue.offerUnsafe(updates, conversation);
  });
  yield* Effect.addFinalizer(() => Effect.sync(unsubscribe));
  yield* Stream.fromQueue(updates).pipe(
    Stream.runForEach((conversation) =>
      SubscriptionRef.update(state, (current) => ({
        ...current,
        conversations: mergeConversation(current.conversations, conversation),
      })),
    ),
    Effect.forkScoped,
  );

  yield* SubscriptionRef.changes(supervisor.state).pipe(
    Stream.runForEach((connection) => {
      switch (connectionProjectionPhase(connection)) {
        case "ready":
          return refresh;
        case "synchronizing":
          return SubscriptionRef.update(state, (current) => ({
            ...current,
            status: current.conversations.length > 0 ? current.status : "synchronizing",
          }));
        case "disconnected":
          return SubscriptionRef.update(state, (current) => ({
            ...current,
            status: current.conversations.length > 0 ? ("cached" as const) : ("empty" as const),
          }));
      }
    }),
    Effect.forkScoped,
  );

  yield* registerListRefresh(refresh);

  return { state, refresh } as const;
});

let activeRefresh: Effect.Effect<void> | undefined;

function registerListRefresh(refresh: Effect.Effect<void>) {
  return Effect.acquireRelease(
    Effect.sync(() => {
      activeRefresh = refresh;
    }),
    () =>
      Effect.sync(() => {
        if (activeRefresh === refresh) activeRefresh = undefined;
      }),
  );
}

/** Refreshes the list if it is mounted; used on navigation. */
export const refreshConversationList: Effect.Effect<void> = Effect.suspend(
  () => activeRefresh ?? Effect.void,
);

export function createConversationListAtoms<R, E>(
  runtime: Atom.AtomRuntime<EnvironmentRegistry | EnvironmentCacheStore | R, E>,
) {
  // One atom per environment, memoised like the detail family. Building a new
  // atom on every call would make every render of the shell subscribe to a new
  // stream, which re-reads the list, which re-renders: an unbounded request
  // loop rather than one live list.
  const family = Atom.family((environmentId: string) =>
    runtime
      .atom(
        followStreamInEnvironment(
          environmentId,
          Stream.unwrap(
            makeConversationListState().pipe(
              Effect.map((handle) => SubscriptionRef.changes(handle.state)),
            ),
          ),
        ),
        { initialValue: EMPTY_CONVERSATION_LIST_STATE },
      )
      .pipe(Atom.withLabel(`conversation-list:${environmentId}`)),
  );
  return { stateAtom: (environmentId: string) => family(environmentId) };
}
