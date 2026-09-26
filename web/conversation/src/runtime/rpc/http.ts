// Same-origin HTTP client for the Detent conversation API.
//
// Written for this repository. It replaces the POC's WebSocket JSON-RPC
// transport with the endpoints in `docs/conversation/decisions.md` §5:
// cookie session for authentication, `X-CSRF-Token` from the bootstrap
// payload on every mutation, and the native `{code, message, details?}`
// error shape decoded into a typed failure.
import * as Effect from "effect/Effect";
import * as Schema from "effect/Schema";

import {
  type ApiError,
  AttachmentUpload,
  Bootstrap,
  type Command,
  type HandoffNext,
  type LinkIssueInput,
  type TurnPreferences,
  ConversationListResponse,
  ConversationSnapshot,
  CreateConversationResponse,
  isApiError,
  isRetryableErrorCode,
  LinkResponse,
  MessagePageResponse,
  Receipt,
  Conversation,
} from "../../contracts/index.ts";

/** A decoded API failure. `retryable` is true only for `503 queue_full`. */
export class ApiRequestError extends Schema.TaggedError<ApiRequestError>()("ApiRequestError", {
  status: Schema.Number,
  code: Schema.String,
  detail: Schema.String,
  retryable: Schema.Boolean,
  /** The failure's `details` member, verbatim. Null when the body had none. */
  details: Schema.Unknown,
}) {
  override get message(): string {
    return this.detail;
  }
}

/** A request that never reached a response: offline, DNS, aborted socket. */
export class ApiTransportError extends Schema.TaggedError<ApiTransportError>()(
  "ApiTransportError",
  { detail: Schema.String },
) {
  override get message(): string {
    return this.detail;
  }
}

export type HttpFailure = ApiRequestError | ApiTransportError;

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface HttpClientOptions {
  /** Absolute or relative origin the API is served from. Empty for same-origin. */
  readonly origin: string;
  /** `api_base` from the bootstrap payload, e.g. `/api/v2/organizations/org_1`. */
  readonly apiBase: string;
  /** CSRF token from the bootstrap payload; sent on every mutation. */
  readonly csrfToken: string;
  readonly fetch?: FetchLike;
}

const MUTATION_METHODS = new Set(["POST", "PATCH", "PUT", "DELETE"]);

function messageForStatus(status: number): string {
  if (status === 401) return "Your session has expired. Sign in again.";
  if (status === 403) return "You do not have permission to do that.";
  if (status === 404) return "That conversation is not available.";
  if (status >= 500) return "The hub could not complete the request.";
  return `The request failed (${status}).`;
}

function errorFromBody(status: number, body: unknown): ApiRequestError {
  if (isApiError(body)) {
    const error = body as ApiError;
    return new ApiRequestError({
      status,
      code: error.code,
      detail: error.message.length > 0 ? error.message : messageForStatus(status),
      retryable: isRetryableErrorCode(error.code),
      details: error.details ?? null,
    });
  }
  return new ApiRequestError({
    status,
    code: status === 404 ? "not_found" : "invalid",
    detail: messageForStatus(status),
    retryable: false,
    details: null,
  });
}

export interface DetentHttpClient {
  readonly origin: string;
  readonly apiBase: string;
  readonly csrfToken: string;
  /** Absolute URL of a conversation event stream, for the SSE transport. */
  readonly eventStreamUrl: (input: {
    readonly projectId: string;
    readonly conversationId: string;
    readonly after: number;
  }) => string;
  readonly bootstrap: Effect.Effect<typeof Bootstrap.Type, HttpFailure>;
  readonly listOrganizationConversations: (input: {
    readonly cursor?: string | null;
    readonly limit?: number;
    readonly q?: string;
    /** `settled=true|false` (decisions.md §14); absent lists both shelves. */
    readonly settled?: boolean;
  }) => Effect.Effect<typeof ConversationListResponse.Type, HttpFailure>;
  readonly listProjectConversations: (input: {
    readonly projectId: string;
    readonly cursor?: string | null;
    readonly limit?: number;
    readonly q?: string;
    /** `settled=true|false` (decisions.md §14); absent lists both shelves. */
    readonly settled?: boolean;
  }) => Effect.Effect<typeof ConversationListResponse.Type, HttpFailure>;
  readonly getConversation: (input: {
    readonly projectId: string;
    readonly conversationId: string;
  }) => Effect.Effect<typeof ConversationSnapshot.Type, HttpFailure>;
  readonly listMessages: (input: {
    readonly projectId: string;
    readonly conversationId: string;
    readonly before: number;
    readonly limit?: number;
  }) => Effect.Effect<typeof MessagePageResponse.Type, HttpFailure>;
  readonly createConversation: (input: {
    readonly projectId: string;
    /**
     * The create key. Mandatory (decisions.md §10.2): creating a conversation
     * runs through the idempotent mutation path keyed by actor and key, so a
     * retried create returns the stored response rather than a second
     * conversation. It is distinct from `firstMessage.key`, which is the
     * message command key.
     */
    readonly key: string;
    readonly title?: string;
    readonly firstMessage?: { readonly key: string; readonly text: string };
  }) => Effect.Effect<typeof CreateConversationResponse.Type, HttpFailure>;
  readonly sendCommand: (input: {
    readonly projectId: string;
    readonly conversationId: string;
    readonly command: Command;
  }) => Effect.Effect<typeof Receipt.Type, HttpFailure>;
  readonly linkConversation: (input: {
    readonly projectId: string;
    readonly conversationId: string;
    readonly key: string;
    /**
     * Sent verbatim. It is never defaulted to `true` here: the confirmation
     * belongs to the user, and a client that silently shares a private
     * history would defeat decisions.md §3 rule 2.
     */
    readonly shareHistory: boolean;
    readonly issue: LinkIssueInput;
    /** The next step for the new issue (decisions.md §13.8, §14). */
    readonly next?: HandoffNext;
  }) => Effect.Effect<typeof LinkResponse.Type, HttpFailure>;
  /**
   * `PATCH /conversations/:id {preferences}` (decisions.md §14). The whole
   * triple is sent every time: a picker changes one field, and the resource
   * carries all three, so a partial body would leave the hub guessing which
   * of the other two the reader meant to keep.
   */
  readonly setPreferences: (input: {
    readonly projectId: string;
    readonly conversationId: string;
    readonly preferences: TurnPreferences;
  }) => Effect.Effect<typeof Conversation.Type, HttpFailure>;
  /** The header's inline rename. The hub's PATCH takes a title, preferences, or both. */
  readonly renameConversation: (input: {
    readonly projectId: string;
    readonly conversationId: string;
    readonly title: string;
  }) => Effect.Effect<typeof Conversation.Type, HttpFailure>;
  /**
   * `POST {nativeBase}/conversations/:id/attachments` (decisions.md §17.1).
   * Multipart with one `file` part and an `idempotency_key` field, exactly as
   * the hub parses it.
   */
  readonly uploadAttachment: (input: {
    readonly projectId: string;
    readonly conversationId: string;
    readonly key: string;
    readonly file: File;
  }) => Effect.Effect<typeof AttachmentUpload.Type, HttpFailure>;
  /** `DELETE .../attachments/:attachment` while the upload is still unsent. */
  readonly deleteAttachment: (input: {
    readonly projectId: string;
    readonly conversationId: string;
    readonly attachmentId: string;
  }) => Effect.Effect<void, HttpFailure>;
}

interface MultipartBody {
  readonly contentType: string;
  readonly bytes: Uint8Array;
}

/**
 * RFC 7578 for exactly the two parts the hub's attachment endpoint reads:
 * `file` (one per request, with its filename and media type) and
 * `idempotency_key` (decisions.md §17.1).
 *
 * The body is assembled here rather than handed to `FormData` so the bytes on
 * the wire are this client's, not the runtime's: the same request is made by a
 * browser and by the test suite, and the boundary, the header casing and the
 * CRLFs are then one thing to read rather than whatever the host happens to
 * emit. A 20 MB cap is the whole payload, so one buffer is the right size.
 */
async function encodeAttachmentUpload(file: File, key: string): Promise<MultipartBody> {
  const boundary = `----detent${Math.random().toString(16).slice(2)}${Date.now().toString(16)}`;
  const encoder = new TextEncoder();
  const media = file.type.length > 0 ? file.type : "application/octet-stream";
  const head = encoder.encode(
    `--${boundary}\r\n` +
      `Content-Disposition: form-data; name="file"; filename="${escapeMultipartValue(file.name)}"\r\n` +
      `Content-Type: ${media}\r\n\r\n`,
  );
  const content = new Uint8Array(await file.arrayBuffer());
  const tail = encoder.encode(
    `\r\n--${boundary}\r\n` +
      `Content-Disposition: form-data; name="idempotency_key"\r\n\r\n` +
      `${key}\r\n` +
      `--${boundary}--\r\n`,
  );
  const bytes = new Uint8Array(head.length + content.length + tail.length);
  bytes.set(head, 0);
  bytes.set(content, head.length);
  bytes.set(tail, head.length + content.length);
  return { contentType: `multipart/form-data; boundary=${boundary}`, bytes };
}

/** Percent-escapes what a quoted `filename` may not contain (RFC 7578 §5.1). */
function escapeMultipartValue(value: string): string {
  return value.replace(/\r/g, "%0D").replace(/\n/g, "%0A").replace(/"/g, "%22");
}

export function makeHttpClient(options: HttpClientOptions): DetentHttpClient {
  const doFetch: FetchLike = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
  const projectBase = (projectId: string) =>
    `${options.apiBase}/projects/${encodeURIComponent(projectId)}`;

  const url = (path: string, query?: Record<string, string | number | boolean | undefined>) => {
    const search = new URLSearchParams();
    for (const [key, value] of Object.entries(query ?? {})) {
      if (value === undefined) continue;
      search.set(key, String(value));
    }
    const suffix = search.size > 0 ? `?${search.toString()}` : "";
    return `${options.origin}${path}${suffix}`;
  };

  function send<A>(
    schema: Schema.Codec<A, any, never, never>,
    method: string,
    target: string,
    body?: unknown,
  ): Effect.Effect<A, HttpFailure> {
    const decode = Schema.decodeUnknownSync(schema);
    return Effect.flatMap(
      Effect.tryPromise({
        try: async () => {
          const headers: Record<string, string> = { Accept: "application/json" };
          if (body !== undefined) headers["Content-Type"] = "application/json";
          if (MUTATION_METHODS.has(method)) headers["X-CSRF-Token"] = options.csrfToken;
          const response = await doFetch(target, {
            method,
            credentials: "same-origin",
            headers,
            ...(body === undefined ? {} : { body: JSON.stringify(body) }),
          });
          const text = await response.text();
          let parsed: unknown = undefined;
          if (text.length > 0) {
            try {
              parsed = JSON.parse(text);
            } catch {
              parsed = undefined;
            }
          }
          return { ok: response.ok, status: response.status, parsed };
        },
        catch: (cause) =>
          new ApiTransportError({
            detail: cause instanceof Error ? cause.message : String(cause),
          }),
      }),
      (result) => {
        if (!result.ok) return Effect.fail(errorFromBody(result.status, result.parsed));
        return Effect.try({
          try: () => decode(result.parsed),
          catch: (cause) =>
            new ApiRequestError({
              status: result.status,
              code: "invalid",
              detail: `The hub returned an unexpected payload: ${
                cause instanceof Error ? cause.message : String(cause)
              }`,
              retryable: false,
              details: null,
            }),
        });
      },
    );
  }

  /**
   * The one request that is not JSON. `send` above sets `Content-Type`, and an
   * upload's is `multipart/form-data` with the boundary that separates its
   * parts, so the upload has its own small transport rather than a flag on the
   * shared one.
   */
  function sendMultipart<A>(
    schema: Schema.Codec<A, any, never, never>,
    target: string,
    body: MultipartBody,
  ): Effect.Effect<A, HttpFailure> {
    const decode = Schema.decodeUnknownSync(schema);
    return Effect.flatMap(
      Effect.tryPromise({
        try: async () => {
          const response = await doFetch(target, {
            method: "POST",
            credentials: "same-origin",
            headers: {
              Accept: "application/json",
              "Content-Type": body.contentType,
              "X-CSRF-Token": options.csrfToken,
            },
            body: body.bytes as unknown as BodyInit,
          });
          const text = await response.text();
          let parsed: unknown = undefined;
          if (text.length > 0) {
            try {
              parsed = JSON.parse(text);
            } catch {
              parsed = undefined;
            }
          }
          return { ok: response.ok, status: response.status, parsed };
        },
        catch: (cause) =>
          new ApiTransportError({
            detail: cause instanceof Error ? cause.message : String(cause),
          }),
      }),
      (result) => {
        if (!result.ok) return Effect.fail(errorFromBody(result.status, result.parsed));
        return Effect.try({
          try: () => decode(result.parsed),
          catch: (cause) =>
            new ApiRequestError({
              status: result.status,
              code: "invalid",
              detail: `The hub returned an unexpected payload: ${
                cause instanceof Error ? cause.message : String(cause)
              }`,
              retryable: false,
              details: null,
            }),
        });
      },
    );
  }

  return {
    origin: options.origin,
    apiBase: options.apiBase,
    csrfToken: options.csrfToken,
    eventStreamUrl: ({ projectId, conversationId, after }) =>
      url(`${projectBase(projectId)}/conversations/${encodeURIComponent(conversationId)}/events`, {
        after,
      }),
    bootstrap: send(Bootstrap, "GET", url("/chat/bootstrap")),
    listOrganizationConversations: (input) =>
      send(
        ConversationListResponse,
        "GET",
        url(`${options.apiBase}/conversations`, {
          cursor: input.cursor ?? undefined,
          limit: input.limit,
          q: input.q,
          settled: input.settled,
        }),
      ),
    listProjectConversations: (input) =>
      send(
        ConversationListResponse,
        "GET",
        url(`${projectBase(input.projectId)}/conversations`, {
          cursor: input.cursor ?? undefined,
          limit: input.limit,
          q: input.q,
          settled: input.settled,
        }),
      ),
    getConversation: (input) =>
      send(
        ConversationSnapshot,
        "GET",
        url(
          `${projectBase(input.projectId)}/conversations/${encodeURIComponent(input.conversationId)}`,
        ),
      ),
    listMessages: (input) =>
      send(
        MessagePageResponse,
        "GET",
        url(
          `${projectBase(input.projectId)}/conversations/${encodeURIComponent(input.conversationId)}/messages`,
          { before: input.before, limit: input.limit },
        ),
      ),
    createConversation: (input) =>
      send(
        CreateConversationResponse,
        "POST",
        url(`${projectBase(input.projectId)}/conversations`),
        {
          key: input.key,
          ...(input.title === undefined ? {} : { title: input.title }),
          ...(input.firstMessage === undefined ? {} : { first_message: input.firstMessage }),
        },
      ),
    sendCommand: (input) =>
      send(
        Receipt,
        "POST",
        url(
          `${projectBase(input.projectId)}/conversations/${encodeURIComponent(input.conversationId)}/commands`,
        ),
        input.command,
      ),
    linkConversation: (input) =>
      send(
        LinkResponse,
        "POST",
        url(
          `${projectBase(input.projectId)}/conversations/${encodeURIComponent(input.conversationId)}/link`,
        ),
        {
          key: input.key,
          share_history: input.shareHistory,
          issue: input.issue,
          ...(input.next === undefined ? {} : { next: input.next }),
        },
      ),
    setPreferences: (input) =>
      send(
        Conversation,
        "PATCH",
        url(
          `${projectBase(input.projectId)}/conversations/${encodeURIComponent(input.conversationId)}`,
        ),
        { preferences: input.preferences },
      ),
    renameConversation: (input) =>
      send(
        Conversation,
        "PATCH",
        url(
          `${projectBase(input.projectId)}/conversations/${encodeURIComponent(input.conversationId)}`,
        ),
        { title: input.title },
      ),
    uploadAttachment: (input) =>
      Effect.flatMap(
        Effect.tryPromise({
          try: () => encodeAttachmentUpload(input.file, input.key),
          catch: (cause) =>
            new ApiTransportError({
              detail: cause instanceof Error ? cause.message : String(cause),
            }),
        }),
        (body) =>
          sendMultipart(
            AttachmentUpload,
            url(
              `${projectBase(input.projectId)}/conversations/${encodeURIComponent(input.conversationId)}/attachments`,
            ),
            body,
          ),
      ),
    deleteAttachment: (input) =>
      Effect.asVoid(
        send(
          Schema.Unknown,
          "DELETE",
          url(
            `${projectBase(input.projectId)}/conversations/${encodeURIComponent(input.conversationId)}/attachments/${encodeURIComponent(input.attachmentId)}`,
          ),
        ),
      ),
  };
}
