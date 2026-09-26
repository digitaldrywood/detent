import * as Effect from "effect/Effect";
import * as Option from "effect/Option";
import * as Queue from "effect/Queue";
import * as Ref from "effect/Ref";
import * as Semaphore from "effect/Semaphore";
import * as Stream from "effect/Stream";
import * as SubscriptionRef from "effect/SubscriptionRef";
import { Atom } from "effect/unstable/reactivity";

import type { ConversationSnapshot } from "../../contracts/index.ts";
import { connectionProjectionPhase } from "../connection/model.ts";
import { EnvironmentRegistry } from "../connection/registry.ts";
import { EnvironmentSupervisor } from "../connection/supervisor.ts";
import { EnvironmentCacheStore } from "../platform/persistence.ts";
import { subscribeDynamic } from "../rpc/client.ts";
import { METHODS, type ConversationStreamItem } from "../rpc/session.ts";
import {
  applyConversationEvent,
  type ConversationDetail,
  type ConversationDetailState,
  type ConversationStatus,
  detailFromSnapshot,
  EMPTY_CONVERSATION_DETAIL_STATE,
  mergeOlderMessages,
} from "./conversationState.ts";
import { conversationBus } from "./conversationList.ts";
import type { ConversationHandles } from "./handles.ts";
import { followStreamInEnvironment } from "./runtime.ts";
import { THREAD_SNAPSHOT_IDLE_TTL_MS } from "./threadRetention.ts";

export interface ConversationKey {
  readonly environmentId: string;
  readonly projectId: string;
  readonly conversationId: string;
}

export function conversationKey(key: ConversationKey): string {
  return JSON.stringify([key.environmentId, key.projectId, key.conversationId]);
}

export function parseConversationKey(key: string): ConversationKey {
  const [environmentId, projectId, conversationId] = JSON.parse(key) as [string, string, string];
  return { environmentId, projectId, conversationId };
}

interface ResumeSnapshot {
  readonly state: ConversationDetailState;
  readonly cursor: number;
}

interface ResumeCache {
  snapshot: ResumeSnapshot | undefined;
  owner: object | undefined;
}

function cachedState(value: ConversationDetailState): ConversationDetailState {
  return {
    ...value,
    status: value.status === "live" && Option.isSome(value.data) ? "live" : "cached",
    error: Option.none(),
  };
}

/** Reasons that end the subscription for good rather than reconnecting. */
const TERMINAL_CLOSE_REASONS = new Set(["access_revoked", "archived"]);

/**
 * Reconnect delay for the nth consecutive failure: one second, doubling, and
 * capped. An unbounded retry at a fixed second is a client that hammers a hub
 * that is already unwell; the cap keeps it coming back without giving up.
 */
export const RECONNECT_BASE_MS = 1_000;
export const RECONNECT_CAP_MS = 30_000;

export function reconnectDelayMs(attempt: number): number {
  if (attempt <= 1) return RECONNECT_BASE_MS;
  return Math.min(RECONNECT_CAP_MS, RECONNECT_BASE_MS * 2 ** (attempt - 1));
}

export const makeConversationDetailState = Effect.fn("ConversationDetail.make")(function* (
  projectId: string,
  conversationId: string,
  handles: ConversationHandles,
  resumeCache?: ResumeCache,
) {
  const supervisor = yield* EnvironmentSupervisor;
  const cache = yield* EnvironmentCacheStore;
  const environmentId = supervisor.target.environmentId;
  const owner = {};
  if (resumeCache) resumeCache.owner = owner;
  const retained = resumeCache?.snapshot;

  const cached =
    retained === undefined
      ? yield* cache
          .loadThread(environmentId, conversationId)
          .pipe(Effect.orElseSucceed(() => Option.none<ConversationSnapshot>()))
      : Option.none<ConversationSnapshot>();

  const initialState: ConversationDetailState =
    retained !== undefined
      ? cachedState(retained.state)
      : Option.match(cached, {
          onNone: () => EMPTY_CONVERSATION_DETAIL_STATE,
          onSome: (snapshot) => ({
            data: Option.some(detailFromSnapshot(snapshot)),
            status: "cached" as const,
            error: Option.none(),
            closedReason: Option.none(),
          }),
        });

  const state = yield* SubscriptionRef.make(initialState);
  const cursor = yield* SubscriptionRef.make(
    retained?.cursor ?? Option.match(cached, { onNone: () => 0, onSome: (s) => s.cursor }),
  );
  const applyLock = yield* Semaphore.make(1);
  // Signals the upstream subscription helper to resubscribe: used after a
  // `cursor_expired` close, which requires a fresh snapshot first.
  const resubscribeSignals = yield* Queue.unbounded<void>();
  const terminated = yield* Ref.make(false);
  // Set when the hub expired our cursor: the next subscription must re-read
  // the snapshot first. It is a flag rather than clearing `data`, because
  // dropping the transcript would unmount the thread and lose the reader's
  // scroll position and the composer draft along with it (U05).
  const needsSnapshot = yield* Ref.make(false);
  // Consecutive failed subscriptions, for the reconnect backoff. Any frame
  // that arrives resets it: the connection proved itself.
  const failures = yield* Ref.make(0);

  let committed: ResumeSnapshot = { state: initialState, cursor: retained?.cursor ?? 0 };
  if (resumeCache?.owner === owner) resumeCache.snapshot = committed;

  const remember = Effect.gen(function* () {
    committed = {
      state: yield* SubscriptionRef.get(state),
      cursor: yield* SubscriptionRef.get(cursor),
    };
    if (resumeCache?.owner === owner) resumeCache.snapshot = committed;
  });

  const setDetail = (update: (detail: ConversationDetail) => ConversationDetail) =>
    SubscriptionRef.update(state, (current) =>
      Option.match(current.data, {
        onNone: () => current,
        onSome: (detail) => ({ ...current, data: Option.some(update(detail)) }),
      }),
    );

  const applyItem = (item: ConversationStreamItem) =>
    applyLock.withPermits(1)(
      Effect.gen(function* () {
        // A revoked or archived conversation stays closed. The supervisor can
        // still hand this subscription a new session, and the hub will close
        // it again; nothing that arrives in between may resurrect the state.
        if (yield* Ref.get(terminated)) return;
        if (item.event.type === "closed") {
          const reason = item.event.data.reason;
          if (reason === "cursor_expired") {
            // The retained window no longer covers this cursor. The cursor is
            // dropped and a fresh snapshot is demanded, but the transcript on
            // screen stays until the snapshot replaces it.
            yield* SubscriptionRef.set(cursor, 0);
            yield* Ref.set(needsSnapshot, true);
            yield* SubscriptionRef.update(state, (current) => ({
              ...current,
              status: "synchronizing" as const,
              closedReason: Option.none(),
            }));
            yield* Queue.offer(resubscribeSignals, undefined);
            return;
          }
          if (reason === "server_error") {
            // The hub could not hold the stream open. It says nothing about
            // the conversation, so the client comes back from the cursor it
            // already holds, on the same backoff a dropped transport uses.
            const attempt = (yield* Ref.updateAndGet(failures, (count) => count + 1)) as number;
            const delay = reconnectDelayMs(attempt);
            yield* SubscriptionRef.update(state, (current) => ({
              ...current,
              status: "synchronizing" as const,
              error: Option.some(
                `The live connection dropped. Reconnecting in ${Math.round(delay / 1000)}s.`,
              ),
              closedReason: Option.none(),
            }));
            yield* Effect.sleep(`${delay} millis`).pipe(
              Effect.andThen(Queue.offer(resubscribeSignals, undefined)),
              Effect.forkScoped,
            );
            return;
          }
          if (TERMINAL_CLOSE_REASONS.has(reason)) yield* Ref.set(terminated, true);
          yield* SubscriptionRef.update(state, (current) => ({
            ...current,
            status: reason === "access_revoked" ? ("gone" as ConversationStatus) : current.status,
            data: reason === "access_revoked" ? Option.none() : current.data,
            closedReason: Option.some(reason),
          }));
          if (reason === "access_revoked") {
            yield* cache.removeThread(environmentId, conversationId).pipe(Effect.ignore);
          }
          return;
        }
        yield* Ref.set(failures, 0);
        if (item.seq !== null) {
          const known = yield* SubscriptionRef.get(cursor);
          if (item.seq <= known) return;
          yield* SubscriptionRef.set(cursor, item.seq);
        }
        // A frame the client could not read only carries the cursor forward.
        if (item.unreadable === true) {
          yield* remember;
          return;
        }
        yield* setDetail((detail) => applyConversationEvent(detail, item.event));
        // The sidebar has no stream of its own this milestone; an open
        // conversation forwards its own updates to the list.
        if (item.event.type === "conversation.updated") {
          const conversation = item.event.data;
          yield* Effect.sync(() => conversationBus.publish(conversation));
        }
        yield* SubscriptionRef.update(state, (current) => ({
          ...current,
          status: Option.isSome(current.data) ? ("live" as const) : current.status,
        }));
      }).pipe(Effect.andThen(remember)),
    );

  const loadSnapshot = Effect.gen(function* () {
    const session = yield* SubscriptionRef.get(supervisor.session);
    if (Option.isNone(session)) return;
    const snapshot = yield* session.value.http
      .getConversation({ projectId, conversationId })
      .pipe(Effect.asSome, Effect.orElseSucceed(() => Option.none<ConversationSnapshot>()));
    if (Option.isNone(snapshot)) {
      yield* SubscriptionRef.update(state, (current) => ({
        ...current,
        status: Option.isSome(current.data) ? current.status : ("empty" as const),
        error: Option.some("This conversation could not be opened."),
      }));
      return;
    }
    yield* SubscriptionRef.set(cursor, snapshot.value.cursor);
    yield* SubscriptionRef.update(state, (current) => ({
      data: Option.some(
        detailFromSnapshot(snapshot.value, Option.getOrUndefined(current.data)),
      ),
      status: "live" as ConversationStatus,
      error: Option.none(),
      closedReason: Option.none(),
    }));
    yield* cache.saveThread(environmentId, snapshot.value).pipe(Effect.ignore);
    yield* remember;
  });

  // Connection phase projects onto the status exactly as upstream does: a
  // disconnected client keeps its transcript and says it is cached.
  yield* SubscriptionRef.changes(supervisor.state).pipe(
    Stream.runForEach((connection) =>
      SubscriptionRef.update(state, (current): ConversationDetailState => {
        if (current.status === "gone") return current;
        switch (connectionProjectionPhase(connection)) {
          case "synchronizing":
            return { ...current, status: "synchronizing" };
          case "disconnected":
            return {
              ...current,
              status: Option.isSome(current.data) ? "cached" : "empty",
            };
          case "ready":
            return current;
        }
      }),
    ),
    Effect.forkScoped,
  );

  yield* Effect.forkScoped(
    subscribeDynamic(
      METHODS.subscribeConversation,
      Effect.fn("ConversationDetail.makeSubscribeInput")(function* () {
        const current = yield* SubscriptionRef.get(state);
        if (Option.isNone(current.data) || (yield* Ref.get(needsSnapshot))) {
          yield* loadSnapshot;
          yield* Ref.set(needsSnapshot, false);
        }
        return {
          projectId,
          conversationId,
          after: yield* SubscriptionRef.get(cursor),
        };
      }),
      {
        onExpectedFailure: (cause) =>
          Effect.gen(function* () {
            const attempt = (yield* Ref.updateAndGet(failures, (count) => count + 1)) as number;
            const delay = reconnectDelayMs(attempt);
            yield* SubscriptionRef.update(state, (current) => ({
              ...current,
              status: Option.isSome(current.data) ? ("cached" as const) : current.status,
              error: Option.some(
                cause.reasons.some((reason) => reason._tag === "Fail")
                  ? `The live connection dropped. Reconnecting in ${Math.round(delay / 1000)}s.`
                  : "The live connection dropped.",
              ),
            }));
            // The retry below waits a fixed tick; the rest of the backoff is
            // waited here, so a hub that keeps failing is asked less often.
            if (delay > RECONNECT_BASE_MS) {
              yield* Effect.sleep(`${delay - RECONNECT_BASE_MS} millis`);
            }
          }),
        retryExpectedFailureAfter: "1 second",
        resubscribe: Stream.fromQueue(resubscribeSignals),
      },
    ).pipe(Stream.runForEach(applyItem)),
  );

  const loadOlder = Effect.gen(function* () {
    const current = yield* SubscriptionRef.get(state);
    if (Option.isNone(current.data)) return;
    const page = current.data.value.page;
    if (!page.hasMore || page.loadingOlder || page.oldestSeq === null) return;
    yield* setDetail((detail) => ({
      ...detail,
      page: { ...detail.page, loadingOlder: true },
    }));
    const session = yield* SubscriptionRef.get(supervisor.session);
    if (Option.isNone(session)) {
      yield* setDetail((detail) => ({ ...detail, page: { ...detail.page, loadingOlder: false } }));
      return;
    }
    const older = yield* session.value.http
      .listMessages({ projectId, conversationId, before: page.oldestSeq, limit: 50 })
      .pipe(Effect.asSome, Effect.orElseSucceed(() => Option.none()));
    yield* setDetail((detail) =>
      Option.match(older, {
        onNone: () => ({ ...detail, page: { ...detail.page, loadingOlder: false } }),
        onSome: (value) => mergeOlderMessages(detail, value.messages, value.next_cursor !== null),
      }),
    );
    yield* remember;
  });

  yield* handles.register(conversationKey({ environmentId, projectId, conversationId }), {
    loadOlder,
    refresh: loadSnapshot.pipe(Effect.andThen(remember)),
    setDetail: (update) => setDetail(update).pipe(Effect.andThen(remember)),
  });

  return { state, loadOlder, setDetail, refresh: loadSnapshot } as const;
});

function conversationStateChanges(
  environmentId: string,
  projectId: string,
  conversationId: string,
  handles: ConversationHandles,
  resumeCache?: ResumeCache,
) {
  return followStreamInEnvironment(
    environmentId,
    Stream.unwrap(
      makeConversationDetailState(projectId, conversationId, handles, resumeCache).pipe(
        Effect.map((handle) => SubscriptionRef.changes(handle.state)),
      ),
    ),
  );
}

export function createConversationDetailAtoms<R, E>(
  runtime: Atom.AtomRuntime<EnvironmentRegistry | EnvironmentCacheStore | R, E>,
  handles: ConversationHandles,
) {
  // The resume definitions outlive the live atoms so back navigation can
  // repaint immediately; upstream's five-minute retention constant is reused
  // unchanged.
  const resumeFamily = Atom.family((key: string) =>
    Atom.make((): ResumeCache => ({ snapshot: undefined, owner: undefined })).pipe(
      Atom.setIdleTTL(THREAD_SNAPSHOT_IDLE_TTL_MS),
      Atom.withLabel(`conversation-resume:${key}`),
    ),
  );

  const family = Atom.family((key: string) => {
    const { environmentId, projectId, conversationId } = parseConversationKey(key);
    const resumeAtom = resumeFamily(key);
    return runtime
      .atom(
        (get) => {
          get.mount(resumeAtom);
          const resume = get.once(resumeAtom);
          const live = conversationStateChanges(
            environmentId,
            projectId,
            conversationId,
            handles,
            resume,
          );
          return resume.snapshot === undefined
            ? live
            : Stream.concat(Stream.succeed(cachedState(resume.snapshot.state)), live);
        },
        { initialValue: EMPTY_CONVERSATION_DETAIL_STATE },
      )
      .pipe(Atom.setIdleTTL(0), Atom.withLabel(`conversation-state:${key}`));
  });

  return {
    stateAtom: (key: ConversationKey) => family(conversationKey(key)),
  };
}
