import * as Cause from "effect/Cause";
import * as Deferred from "effect/Deferred";
import * as Effect from "effect/Effect";
import * as Queue from "effect/Queue";
import * as Stream from "effect/Stream";

import {
  type ConversationEvent,
  decodeEventFrame,
} from "../../contracts/index.ts";
import { ConnectionTransientError } from "../connection/model.ts";
import { type DetentHttpClient } from "./http.ts";
import { defaultSseTransport, HEARTBEAT_TIMEOUT_MS, type SseTransport } from "./sse.ts";

/** One item from a conversation subscription. `seq` is null for heartbeats. */
export interface ConversationStreamItem {
  readonly seq: number | null;
  readonly event: ConversationEvent;
  /**
   * True for a frame this client could not read. It carries the frame's seq
   * and a heartbeat body so the cursor still advances past it: a frame the
   * client cannot decode is one frame lost, and tearing the stream down over
   * it produced an endless reconnect from an unchanged cursor that re-fetched
   * the same unreadable frame every second.
   */
  readonly unreadable?: boolean;
}

export class ConversationStreamError extends Error {
  readonly _tag = "ConversationStreamError";
  constructor(detail: string) {
    super(detail);
    this.name = "ConversationStreamError";
  }
}

export interface SubscribeConversationInput {
  readonly projectId: string;
  readonly conversationId: string;
  readonly after: number;
}

export interface DetentHubMethods {
  readonly subscribeConversation: (
    input: SubscribeConversationInput,
  ) => Stream.Stream<ConversationStreamItem, ConversationStreamError>;
}

export interface RpcSession {
  readonly client: DetentHubMethods;
  readonly http: DetentHttpClient;
  readonly probe: Effect.Effect<void, ConnectionTransientError>;
  readonly closed: Effect.Effect<never, ConnectionTransientError>;
}

/** Method names, mirroring the POC's `METHODS` table. */
export const METHODS = {
  subscribeConversation: "subscribeConversation",
} as const;

export interface SessionOptions {
  readonly http: DetentHttpClient;
  readonly transport?: SseTransport;
  readonly heartbeatTimeoutMs?: number;
}

/**
 * Opens a session. `Effect.addFinalizer` is not needed: nothing is held open
 * until a subscription starts, and each subscription owns its own transport.
 */
export const connectSession = (options: SessionOptions) =>
  Effect.gen(function* () {
    const transport = options.transport ?? defaultSseTransport();
    const heartbeatTimeoutMs = options.heartbeatTimeoutMs ?? HEARTBEAT_TIMEOUT_MS;
    const failure = yield* Deferred.make<never, ConnectionTransientError>();

    const reportTransportFailure = (detail: string) =>
      Deferred.doneUnsafe(
        failure,
        Effect.fail(new ConnectionTransientError({ reason: "transport", detail })),
      );

    const subscribeConversation = (input: SubscribeConversationInput) =>
      Stream.callback<ConversationStreamItem, ConversationStreamError>((queue) =>
        Effect.gen(function* () {
          let cursor = input.after;
          let watchdog: ReturnType<typeof setTimeout> | undefined;
          let dispose: (() => void) | undefined;
          let finished = false;

          const stop = () => {
            if (watchdog !== undefined) clearTimeout(watchdog);
            watchdog = undefined;
            dispose?.();
          };
          const failStream = (detail: string, transportDead: boolean) => {
            if (finished) return;
            finished = true;
            stop();
            if (transportDead) reportTransportFailure(detail);
            Queue.failCauseUnsafe(queue, Cause.fail(new ConversationStreamError(detail)));
          };
          const arm = () => {
            if (watchdog !== undefined) clearTimeout(watchdog);
            watchdog = setTimeout(() => {
              // Heartbeats arrive every 15 seconds. Three missed in a row is a
              // stream that is open at the socket layer and dead above it.
              failStream("The event stream stopped sending heartbeats.", true);
            }, heartbeatTimeoutMs);
            // Node keeps the process alive for a pending timer; this one must not.
            (watchdog as unknown as { unref?: () => void }).unref?.();
          };

          dispose = transport(
            options.http.eventStreamUrl({
              projectId: input.projectId,
              conversationId: input.conversationId,
              after: cursor,
            }),
            {
              onFrame: (frame) => {
                if (finished) return;
                arm();
                const seq =
                  frame.id === null || !Number.isFinite(Number(frame.id))
                    ? null
                    : Number(frame.id);
                if (seq !== null) cursor = seq;
                let event: ConversationEvent;
                try {
                  event = decodeEventFrame(frame.event, JSON.parse(frame.data));
                } catch (cause) {
                  // Skipped, not fatal. The reader loses one frame; the stream
                  // and the cursor carry on, and the next snapshot repairs
                  // whatever the frame would have said.
                  console.warn(
                    `Skipping an event this client cannot read: ${
                      cause instanceof Error ? cause.message : String(cause)
                    }`,
                  );
                  Queue.offerUnsafe(queue, {
                    seq,
                    event: { type: "heartbeat", data: { seq: seq ?? 0 } },
                    unreadable: true,
                  });
                  return;
                }
                Queue.offerUnsafe(queue, { seq, event });
                if (event.type === "closed") {
                  // A `closed` frame is a decision for the conversation state,
                  // not a transport fault: end the stream without reconnecting.
                  finished = true;
                  stop();
                  Queue.endUnsafe(queue);
                }
              },
              onError: (detail) => failStream(detail, true),
            },
          );
          arm();
          yield* Effect.addFinalizer(() =>
            Effect.sync(() => {
              finished = true;
              stop();
            }),
          );
        }),
      );

    const session: RpcSession = {
      client: { subscribeConversation },
      http: options.http,
      probe: options.http.bootstrap.pipe(
        Effect.asVoid,
        Effect.catch((error) =>
          Effect.fail(
            new ConnectionTransientError({
              reason: error._tag === "ApiTransportError" ? "network" : "endpoint-unavailable",
              detail: error.message,
            }),
          ),
        ),
      ),
      closed: Deferred.await(failure),
    };
    return session;
  });
