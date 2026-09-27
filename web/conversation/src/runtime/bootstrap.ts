import * as Effect from "effect/Effect";
import * as Layer from "effect/Layer";
import * as Option from "effect/Option";
import * as Schema from "effect/Schema";
import * as Stream from "effect/Stream";
import { Atom } from "effect/unstable/reactivity";

import { AccountBootstrap } from "../contracts/account.ts";
import {
  type Answers,
  Bootstrap,
  type Command,
  HUB_ENVIRONMENT_ID,
  type HandoffNext,
  type LinkIssueInput,
  type Receipt,
  type TurnPreferences,
} from "../contracts/index.ts";
import { PrimaryConnectionRegistration } from "./connection/catalog.ts";
import { Connectivity } from "./connection/connectivity.ts";
import { ConnectionCredentialStore } from "./connection/credentialStore.ts";
import { ConnectionDriver } from "./connection/driver.ts";
import { ConnectionBlockedError, PrimaryConnectionTarget } from "./connection/model.ts";
import { ConnectionProfileStore } from "./connection/profileStore.ts";
import * as Registry from "./connection/registry.ts";
import { ConnectionWakeups } from "./connection/wakeups.ts";
import { SshEnvironmentGateway } from "./platform/capabilities.ts";
import * as Persistence from "./platform/persistence.ts";
import { type DetentHttpClient, makeHttpClient } from "./rpc/http.ts";
import { connectSession } from "./rpc/session.ts";
import type { SseTransport } from "./rpc/sse.ts";
import {
  conversationBus,
  createConversationListAtoms,
  refreshConversationList,
} from "./state/conversationList.ts";
import {
  applyConversationEvent,
  type ConversationDetail,
  type ControlKind,
  type PendingControl,
  type PendingMessage,
  withControl,
  withoutControl,
  withPending,
  withoutPending,
  withStaleExecution,
} from "./state/conversationState.ts";
import { conversationKey, createConversationDetailAtoms } from "./state/conversations.ts";
import { DraftStore } from "./state/drafts.ts";
import { ConversationHandles } from "./state/handles.ts";
import { applyHubPaths, hubPath } from "./basePath.ts";

export interface ClientOptions {
  /** Origin the API is served from. Empty string for same-origin. */
  readonly origin?: string;
  readonly bootstrap: typeof Bootstrap.Type;
  /**
   * The extended payload the account screens read. It defaults to whatever the
   * last `loadBootstrap` decoded, so the entry point needs no change; a test
   * that builds a client by hand passes its own.
   */
  readonly account?: typeof AccountBootstrap.Type | null;
  readonly transport?: SseTransport;
  readonly fetch?: typeof globalThis.fetch;
  readonly heartbeatTimeoutMs?: number;
}

/**
 * The extended payload from the last `loadBootstrap`, where the hub served
 * one. `/app/bootstrap` is a superset of `/chat/bootstrap`
 * (decisions.md §12, "Serving"): the same object decodes through both schemas,
 * so the account screens read the wider view of the payload the shell already
 * fetched rather than asking for it a second time. It is `null` against a hub
 * that still serves only the chat payload.
 */
let latestAccount: typeof AccountBootstrap.Type | null = null;

/** The extended bootstrap the last load saw, or null. */
export function lastAccountBootstrap(): typeof AccountBootstrap.Type | null {
  return latestAccount;
}

const decodeAccount = Schema.decodeUnknownOption(AccountBootstrap);

/**
 * A bootstrap the hub refused. The status and the decoded body travel with it
 * because the two routes that render without a session need them: `/login`
 * treats 401 and 403 as the expected state, and `/support` reads the
 * organization out of a 403 body where the hub names one (decisions.md §12).
 */
export class BootstrapRefused extends Error {
  readonly status: number;
  readonly body: unknown;
  constructor(status: number, body: unknown) {
    super(`The chat session could not be started (${status}).`);
    this.name = "BootstrapRefused";
    this.status = status;
    this.body = body;
  }
}

/** Loads the bootstrap payload. Everything else needs its CSRF token first. */
export async function loadBootstrap(
  origin = "",
  fetchImpl: typeof globalThis.fetch = globalThis.fetch,
): Promise<typeof Bootstrap.Type> {
  const request = (path: string) =>
    fetchImpl(`${origin}${hubPath(path)}`, {
      credentials: "same-origin",
      headers: { Accept: "application/json" },
    });
  // `/chat/bootstrap` is kept as an alias, so a hub that has not moved yet
  // still answers. Only a 404 falls back: any other failure is the failure.
  let response = await request("/app/bootstrap");
  if (response.status === 404) response = await request("/chat/bootstrap");
  if (!response.ok) {
    let body: unknown = undefined;
    try {
      body = await response.json();
    } catch {
      body = undefined;
    }
    throw new BootstrapRefused(response.status, body);
  }
  const payload: unknown = await response.json();
  const account = decodeAccount(payload);
  latestAccount = account._tag === "Some" ? account.value : null;
  const bootstrap = Schema.decodeUnknownSync(Bootstrap)(payload);
  applyHubPaths(bootstrap);
  return bootstrap;
}

export interface SendMessageInput {
  readonly projectId: string;
  readonly conversationId: string;
  readonly key: string;
  readonly text: string;
  /**
   * Ids of attachments already uploaded against this conversation and not yet
   * sent (decisions.md §17.1). Omitted where there are none, so a message
   * without files is the same command it always was.
   */
  readonly attachments?: readonly string[];
  readonly expected?: { readonly attempt_id: string | null; readonly turn_id: string | null };
}

export interface RetryMessageInput {
  readonly projectId: string;
  readonly conversationId: string;
  /** A new key every time: a retry is its own command (decisions.md §10.3). */
  readonly key: string;
  readonly messageId: string;
}

export function makeClient(options: ClientOptions) {
  const origin = options.origin ?? "";
  const http = makeHttpClient({
    origin,
    apiBase: options.bootstrap.api_base,
    csrfToken: options.bootstrap.csrf_token,
    fetch: options.fetch,
  });

  const target = new PrimaryConnectionTarget({
    environmentId: HUB_ENVIRONMENT_ID,
    label: "Detent hub",
    httpBaseUrl: origin,
    // Kept for the unmodified upstream target shape; the SSE transport builds
    // its own URLs from the API base and never reads this.
    wsBaseUrl: origin,
  });

  const cache = Persistence.memoryCache();
  const drafts = new DraftStore();
  drafts.reconcileAccount(
    `${options.bootstrap.organization.id}:${options.bootstrap.actor.principal_id}`,
  );

  const services = Layer.mergeAll(
    Layer.succeed(Persistence.ConnectionTargetStore, { list: Effect.succeed([]) }),
    Layer.succeed(Persistence.ConnectionRegistrationStore, {
      register: () => Effect.void,
      remove: () => Effect.void,
    }),
    Layer.succeed(Persistence.EnvironmentCacheStore, cache),
    Layer.succeed(Persistence.EnvironmentOwnedDataCleanup, { clear: () => Effect.void }),
    Layer.succeed(ConnectionProfileStore, {
      get: () => Effect.succeed(Option.none()),
      put: () => Effect.void,
      remove: () => Effect.void,
    }),
    Layer.succeed(ConnectionCredentialStore, {
      get: () => Effect.succeed(Option.none()),
      put: () => Effect.void,
      remove: () => Effect.void,
    }),
    Layer.succeed(Connectivity, {
      status: Effect.succeed("online" as const),
      changes: Stream.never,
    }),
    Layer.succeed(ConnectionWakeups, { changes: Stream.never }),
    Layer.succeed(SshEnvironmentGateway, { disconnect: () => Effect.void }),
    Layer.succeed(ConnectionDriver, {
      connect: (entry, report) =>
        Effect.gen(function* () {
          if (entry.target._tag !== "PrimaryConnectionTarget") {
            return yield* new ConnectionBlockedError({
              reason: "unsupported",
              detail: "The conversation client only connects to its own hub.",
            });
          }
          const prepared = {
            environmentId: entry.target.environmentId,
            label: entry.target.label,
            httpBaseUrl: entry.target.httpBaseUrl,
            socketUrl: entry.target.wsBaseUrl,
            httpAuthorization: null,
            target: entry.target,
          };
          yield* report({ stage: "opening", prepared });
          const session = yield* connectSession({
            http,
            transport: options.transport,
            heartbeatTimeoutMs: options.heartbeatTimeoutMs,
          });
          yield* report({ stage: "synchronizing", prepared });
          return { prepared, session };
        }),
    }),
  );

  const registered = Layer.effect(
    Registry.EnvironmentRegistry,
    Effect.gen(function* () {
      const registry = yield* Registry.make;
      yield* registry.registerPlatform(new PrimaryConnectionRegistration({ target }));
      return registry;
    }),
  ).pipe(Layer.provideMerge(services));

  const runtime = Atom.runtime(registered);
  const handles = new ConversationHandles();
  const list = createConversationListAtoms(runtime);
  const conversations = createConversationDetailAtoms(runtime, handles);

  const keyOf = (projectId: string, conversationId: string) =>
    conversationKey({ environmentId: HUB_ENVIRONMENT_ID, projectId, conversationId });

  /**
   * Sends a command with an optimistic entry that only ever resolves through
   * the receipt. A request that never produced an outcome leaves the entry as
   * `unknown`; nothing here retries it (decisions.md §5, delivery ladder).
   */
  const dispatch = (
    projectId: string,
    conversationId: string,
    command: Command,
    optimistic: PendingMessage | null,
  ): Effect.Effect<Receipt | null> =>
    Effect.gen(function* () {
      const key = keyOf(projectId, conversationId);
      // The stream and this response both report on the same command, and the
      // stream can win. A message that is already in the transcript under this
      // key needs no bubble: adding one would show the reader their message
      // twice, once as history and once as "not yet confirmed".
      const accepted = (detail: ConversationDetail, commandKey: string) =>
        detail.messages.some((message) => message.command_key === commandKey);
      if (optimistic !== null) {
        yield* handles.update(key, (detail) =>
          accepted(detail, optimistic.key) ? detail : withPending(detail, optimistic),
        );
      }
      const outcome = yield* http
        .sendCommand({ projectId, conversationId, command })
        .pipe(Effect.result);
      if (outcome._tag === "Success") {
        const receipt = outcome.success;
        yield* handles.update(key, (detail) =>
          applyConversationEvent(detail, { type: "command.receipt", data: receipt }),
        );
        return receipt;
      }
      const error = outcome.failure;
      const unknownOutcome = error._tag === "ApiTransportError" || error.status >= 500;
      yield* handles.update(key, (detail) =>
        optimistic === null || accepted(detail, optimistic.key)
          ? detail
          : withPending(detail, {
              ...optimistic,
              status: unknownOutcome && !(error._tag === "ApiRequestError" && error.retryable)
                ? "unknown"
                : "failed",
              error: error.message,
              errorCode: error._tag === "ApiRequestError" ? error.code : "network",
              retryable: error._tag === "ApiRequestError" && error.retryable,
            }),
      );
      return null;
    });

  /** Re-reads the snapshot of an open conversation, if it is open. */
  const refreshConversation = (projectId: string, conversationId: string) =>
    Effect.suspend(() => {
      const handle = handles.get(keyOf(projectId, conversationId));
      return handle === undefined ? Effect.void : handle.refresh;
    });

  /**
   * Sends an answer, interrupt or continue. The outbox entry is the client's
   * record of the intent: it settles on the receipt, becomes `unknown` when no
   * outcome was established, and is never resent on its own. A
   * `stale_execution` failure raises the notice and re-reads the snapshot; it
   * never re-aims the control at the attempt that replaced the one the user
   * was looking at (decisions.md §2).
   */
  const dispatchControl = (
    projectId: string,
    conversationId: string,
    command: Command,
    kind: ControlKind,
    questionId: string | null,
  ): Effect.Effect<Receipt | null> =>
    Effect.gen(function* () {
      const key = keyOf(projectId, conversationId);
      const attemptId = command.expected?.attempt_id ?? null;
      const optimistic: PendingControl = {
        key: command.key,
        kind,
        questionId,
        attemptId,
        createdAt: new Date().toISOString(),
        status: "sending",
        error: null,
        errorCode: null,
        receiptStatus: null,
        // Retrying a control resends the identical payload under the same key,
        // so the payload is kept here rather than in the view: the view can be
        // unmounted and remounted, and the outbox entry outlives it.
        expected: command.expected ?? null,
        answers: command.kind === "answer" ? command.answers : null,
      };
      yield* handles.update(key, (detail) => withControl(detail, optimistic));
      const outcome = yield* http
        .sendCommand({ projectId, conversationId, command })
        .pipe(Effect.result);
      if (outcome._tag === "Success") {
        const receipt = outcome.success;
        yield* handles.update(key, (detail) =>
          applyConversationEvent(detail, { type: "command.receipt", data: receipt }),
        );
        return receipt;
      }
      const error = outcome.failure;
      const stale = error._tag === "ApiRequestError" && error.code === "stale_execution";
      const unknownOutcome = error._tag === "ApiTransportError" || error.status >= 500;
      yield* handles.update(key, (detail) =>
        withStaleExecution(
          withControl(detail, {
            ...optimistic,
            status: unknownOutcome && !(error._tag === "ApiRequestError" && error.retryable)
              ? "unknown"
              : "rejected",
            error: error.message,
            errorCode: error._tag === "ApiRequestError" ? error.code : "network",
          }),
          stale || detail.staleExecution,
        ),
      );
      if (stale) yield* refreshConversation(projectId, conversationId);
      return null;
    });

  const effects = {
    sendMessage: (input: SendMessageInput) =>
      dispatch(
        input.projectId,
        input.conversationId,
        {
          key: input.key,
          kind: "message",
          text: input.text,
          ...(input.attachments === undefined || input.attachments.length === 0
            ? {}
            : { attachments: input.attachments }),
          ...(input.expected === undefined ? {} : { expected: input.expected }),
        },
        {
          key: input.key,
          text: input.text,
          attachments: input.attachments ?? null,
          createdAt: new Date().toISOString(),
          messageId: null,
          expected: input.expected ?? null,
          receiptStatus: null,
          status: "sending" as const,
          error: null,
          errorCode: null,
          retryable: false,
        },
      ),
    sendControl: (input: { projectId: string; conversationId: string; command: Command }) =>
      dispatch(input.projectId, input.conversationId, input.command, null),
    sendAnswer: (input: {
      projectId: string;
      conversationId: string;
      key: string;
      questionId: string;
      answers: Answers;
      expected: { attempt_id: string | null; turn_id: string | null };
    }) =>
      dispatchControl(
        input.projectId,
        input.conversationId,
        {
          key: input.key,
          kind: "answer",
          question_id: input.questionId,
          answers: input.answers,
          expected: input.expected,
        },
        "answer",
        input.questionId,
      ),
    sendInterrupt: (input: {
      projectId: string;
      conversationId: string;
      key: string;
      expected: { attempt_id: string | null; turn_id: string | null };
    }) =>
      dispatchControl(
        input.projectId,
        input.conversationId,
        { key: input.key, kind: "interrupt", expected: input.expected },
        "interrupt",
        null,
      ),
    sendContinue: (input: {
      projectId: string;
      conversationId: string;
      key: string;
      text?: string;
      expected: { attempt_id: string | null; turn_id: string | null };
    }) =>
      dispatchControl(
        input.projectId,
        input.conversationId,
        {
          key: input.key,
          kind: "continue",
          ...(input.text === undefined ? {} : { text: input.text }),
          expected: input.expected,
        },
        "continue",
        null,
      ),
    /**
     * Re-queues one message the hub already stored (decisions.md §10.3). It is
     * a command of its own: a new key every time, naming the message, and the
     * hub sets that same message back to `queued` (or `saved`) and emits
     * `message.updated`. Nothing is duplicated, so this never creates a second
     * message however often it is pressed.
     */
    retryMessage: (input: RetryMessageInput) =>
      dispatch(
        input.projectId,
        input.conversationId,
        { key: input.key, kind: "retry", message_id: input.messageId },
        null,
      ),
    /**
     * Retries an optimistic entry. Once a receipt named the message the entry
     * became, this is the `retry` command above. Before that no message exists
     * to name, so the only recovery is to re-send the byte identical `message`
     * command under its original key: the hub returns the stored receipt if it
     * did land, and accepts it if it did not. `expected` travels with it —
     * dropping it would be a different payload under the same key, which is
     * `idempotency_conflict`.
     */
    retryPending: (input: {
      projectId: string;
      conversationId: string;
      key: string;
      entry: PendingMessage;
    }) =>
      input.entry.messageId === null
        ? dispatch(
            input.projectId,
            input.conversationId,
            {
              key: input.entry.key,
              kind: "message",
              text: input.entry.text,
              ...(input.entry.attachments === null || input.entry.attachments.length === 0
                ? {}
                : { attachments: input.entry.attachments }),
              ...(input.entry.expected === null ? {} : { expected: input.entry.expected }),
            },
            { ...input.entry, status: "sending" as const, error: null, errorCode: null },
          )
        : dispatch(
            input.projectId,
            input.conversationId,
            { key: input.key, kind: "retry", message_id: input.entry.messageId },
            { ...input.entry, status: "sending" as const, error: null, errorCode: null },
          ).pipe(
            // The retry receipt is keyed by the retry command, not by the
            // entry, so the entry is moved on here. It is only updated if it
            // is still there: the `message.updated` the hub emits retires it
            // like any accepted message, and re-adding it afterwards would
            // leave a bubble for a message that is already in the transcript.
            Effect.tap((receipt) =>
              receipt === null || receipt.status === "unknown" || receipt.status === "rejected"
                ? Effect.void
                : handles.update(keyOf(input.projectId, input.conversationId), (detail) =>
                    detail.pending.some((entry) => entry.key === input.entry.key)
                      ? withPending(detail, {
                          ...input.entry,
                          status: "queued",
                          error: null,
                          errorCode: null,
                        })
                      : detail,
                  ),
            ),
          ),
    /**
     * The composer's model, effort and access pickers (decisions.md §13.14).
     * The updated conversation is merged into the open detail and forwarded to
     * the sidebar, so a picker changes what the next turn runs under without a
     * refresh.
     */
    setPreferences: (input: {
      projectId: string;
      conversationId: string;
      preferences: TurnPreferences;
    }) =>
      http.setPreferences(input).pipe(
        Effect.tap((conversation) =>
          handles.update(keyOf(input.projectId, input.conversationId), (detail) =>
            applyConversationEvent(detail, {
              type: "conversation.updated",
              data: conversation,
            }),
          ).pipe(Effect.andThen(Effect.sync(() => conversationBus.publish(conversation)))),
        ),
        Effect.result,
      ),

    renameConversation: (input: {
      projectId: string;
      conversationId: string;
      title: string;
    }) =>
      http.renameConversation(input).pipe(
        Effect.tap((conversation) =>
          handles.update(keyOf(input.projectId, input.conversationId), (detail) =>
            applyConversationEvent(detail, {
              type: "conversation.updated",
              data: conversation,
            }),
          ).pipe(Effect.andThen(Effect.sync(() => conversationBus.publish(conversation)))),
        ),
        Effect.result,
      ),
    /**
     * Resends one outbox control exactly as it was sent: same key, same
     * payload. Everything it needs is on the entry, so a reader who navigated
     * away and back can still retry it.
     */
    retryControl: (input: {
      projectId: string;
      conversationId: string;
      entry: PendingControl;
    }) =>
      dispatchControl(
        input.projectId,
        input.conversationId,
        input.entry.kind === "answer"
          ? {
              key: input.entry.key,
              kind: "answer",
              question_id: input.entry.questionId ?? "",
              answers: input.entry.answers ?? {},
              ...(input.entry.expected === null ? {} : { expected: input.entry.expected }),
            }
          : {
              key: input.entry.key,
              kind: input.entry.kind,
              ...(input.entry.expected === null ? {} : { expected: input.entry.expected }),
            },
        input.entry.kind,
        input.entry.questionId,
      ),
    discardControl: (input: { projectId: string; conversationId: string; key: string }) =>
      handles.update(keyOf(input.projectId, input.conversationId), (detail) =>
        withoutControl(detail, input.key),
      ),
    acknowledgeStale: (input: { projectId: string; conversationId: string }) =>
      handles.update(keyOf(input.projectId, input.conversationId), (detail) =>
        withStaleExecution(detail, false),
      ),
    refreshConversation: (input: { projectId: string; conversationId: string }) =>
      refreshConversation(input.projectId, input.conversationId),
    dismissPending: (input: { projectId: string; conversationId: string; key: string }) =>
      handles.update(keyOf(input.projectId, input.conversationId), (detail) =>
        withoutPending(detail, input.key),
      ),
    createConversation: (input: {
      projectId: string;
      key: string;
      title?: string;
      firstMessage?: { key: string; text: string };
    }) => http.createConversation(input).pipe(Effect.result),
    loadOlder: (input: { projectId: string; conversationId: string }) =>
      Effect.suspend(() => {
        const handle = handles.get(keyOf(input.projectId, input.conversationId));
        return handle === undefined ? Effect.void : handle.loadOlder;
      }),
    refreshList: () => refreshConversationList,
    searchConversations: (input: { query: string }) =>
      http.listOrganizationConversations({ q: input.query, limit: 30 }).pipe(Effect.result),
    /**
     * Creates the linked issue. The key is the caller's: it is generated once
     * per handoff intent and reused by every retry, so a link that landed
     * cannot produce a second issue. On success the conversation resource is
     * merged into the open detail and forwarded to the sidebar, so the route
     * and the history stay exactly where they are (U06).
     */
    link: (input: {
      projectId: string;
      conversationId: string;
      key: string;
      shareHistory: boolean;
      issue: LinkIssueInput;
      next?: HandoffNext;
    }) =>
      http.linkConversation(input).pipe(
        Effect.tap((response) =>
          handles.update(keyOf(input.projectId, input.conversationId), (detail) =>
            applyConversationEvent(detail, {
              type: "conversation.updated",
              data: response.conversation,
            }),
          ).pipe(
            Effect.andThen(
              Effect.sync(() => conversationBus.publish(response.conversation)),
            ),
          ),
        ),
        Effect.result,
      ),
  } as const;

  return {
    runtime,
    layer: registered,
    handles,
    http: http as DetentHttpClient,
    bootstrap: options.bootstrap,
    account: options.account ?? latestAccount,
    drafts,
    list,
    conversations,
    keyOf,
    /** Raw effects. Every one is `R = never`, so tests can run them directly. */
    effects,
    sendMessage: runtime.fn(effects.sendMessage),
    retryMessage: runtime.fn(effects.retryMessage),
    retryControl: runtime.fn(effects.retryControl),
    retryPending: runtime.fn(effects.retryPending),
    setPreferences: runtime.fn(effects.setPreferences),
    renameConversation: runtime.fn(effects.renameConversation),
    sendControl: runtime.fn(effects.sendControl),
    sendAnswer: runtime.fn(effects.sendAnswer),
    sendInterrupt: runtime.fn(effects.sendInterrupt),
    sendContinue: runtime.fn(effects.sendContinue),
    discardControl: runtime.fn(effects.discardControl),
    acknowledgeStale: runtime.fn(effects.acknowledgeStale),
    refreshConversation: runtime.fn(effects.refreshConversation),
    dismissPending: runtime.fn(effects.dismissPending),
    createConversation: runtime.fn(effects.createConversation),
    loadOlder: runtime.fn(effects.loadOlder),
    refreshList: runtime.fn(effects.refreshList),
    searchConversations: runtime.fn(effects.searchConversations),
    link: runtime.fn(effects.link),
  };
}

export type ConversationClient = ReturnType<typeof makeClient>;
