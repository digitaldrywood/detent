/**
 * The channels §18.2 defines, plus `exec` (§18.12) and `git` (§18.13).
 *
 * `files` and `git` are implemented by this module's request/response client,
 * because both are one request answered by one frame. `exec` is implemented
 * beside it, in `adapters/actionRuns.ts`, whose header says why a run — which
 * streams output for as long as it lasts — is not the same shape.
 */
export type RelayChannel = "terminal" | "files" | "diff" | "preview" | "exec" | "git";

/** Every error code the relay, the files channel and the git channel answer with. */
export const RELAY_ERROR_CODES = [
  "not_found",
  "forbidden",
  "too_large",
  "denied",
  "unsupported",
  "unknown_frame",
  "invalid_frame",
  "stream_limit",
  "relay_busy",
  "resume_failed",
  "revoked",
  "superseded",
  "stale_execution",
  "overflow",
  "workspace_closed",
  "read_only",
  // §18.12: git itself refused. The only code that carries `stderr`, because
  // a failed push is only actionable with the tool's own words.
  "git_failed",
] as const;
export type RelayErrorCode = (typeof RELAY_ERROR_CODES)[number];

const RELAY_ERROR_CODE_SET: ReadonlySet<string> = new Set(RELAY_ERROR_CODES);

/** A code the hub sent that this build does not know is reported as-is. */
export function isRelayErrorCode(value: string): value is RelayErrorCode {
  return RELAY_ERROR_CODE_SET.has(value);
}

/**
 * A relay failure.
 *
 * `code` is the wire code rather than a translated message, because every
 * caller that acts on one acts on the code: `denied` and `too_large` each have
 * their own state in the Files surface, and everything else is reported.
 */
export class RelayError extends Error {
  readonly code: string;
  /** The request `seq` the hub named, where it named one. */
  readonly requestSeq: number | null;
  /**
   * The tool's own standard error, on a `git_failed` (§18.12), else null.
   *
   * Carried rather than folded into `message` because the two are read
   * differently: `message` is the sentence for the reader, and this is the
   * output a caller picks the actionable line out of. A push writes progress
   * before the reason, so `headerGit.ts` takes the last non-empty line.
   */
  readonly stderr: string | null;

  constructor(
    code: string,
    message?: string,
    requestSeq?: number | null,
    stderr?: string | null,
  ) {
    super(message !== undefined && message.length > 0 ? message : relayErrorMessage(code));
    this.name = "RelayError";
    this.code = code;
    this.requestSeq = requestSeq ?? null;
    this.stderr = stderr ?? null;
  }
}

/** The one place a wire code becomes a sentence a reader sees. */
export function relayErrorMessage(code: string): string {
  switch (code) {
    case "not_found":
      return "That path is no longer in the worktree.";
    case "forbidden":
      return "The runner refused to read that path.";
    case "too_large":
      return "This file is larger than the 2 MB read limit.";
    case "denied":
      return "This path is on the workspace's denylist.";
    case "unsupported":
      return "The runner does not support that request.";
    case "unknown_frame":
    case "invalid_frame":
      return "The runner rejected the request.";
    case "stream_limit":
      return "This workspace has no free streams left.";
    case "relay_busy":
      return "The relay is at capacity. Try again shortly.";
    case "resume_failed":
      return "The connection could not be resumed.";
    case "revoked":
      return "Your access to this workspace changed.";
    case "superseded":
      return "Another runner took over this workspace.";
    case "stale_execution":
      return "The runner lost its lease on this worktree.";
    case "overflow":
      return "The runner sent more than the relay could hold.";
    case "workspace_closed":
      return "This workspace has closed.";
    case "read_only":
      return "This workspace is read-only.";
    case "git_failed":
      return "The runner's version control refused the command.";
    case "already_running":
      // §18.12: the hub handed this run to the runner itself, because nobody
      // opened the exec channel for it in time. It is running; the surface
      // reads it back from the run rather than starting it again.
      return "This run is already being executed; its output is read back from the run.";
    default:
      return "The relay reported an error.";
  }
}

// --- Frames -----------------------------------------------------------------

export interface RelayFrame {
  readonly channel: string;
  readonly stream?: string | undefined;
  readonly type: string;
  readonly seq?: number | undefined;
  readonly payload?: unknown;
}

export interface FileEntry {
  readonly name: string;
  readonly kind: "file" | "dir" | "symlink";
  readonly size: number;
  readonly modified_at: string;
  readonly ignored: boolean;
  readonly denied: boolean;
}

export interface ListedPayload {
  readonly path: string;
  readonly entries: readonly FileEntry[];
  readonly next_cursor?: string | null;
}

export interface StatPayload {
  readonly path: string;
  readonly kind: "file" | "dir" | "symlink";
  readonly size: number;
  readonly modified_at: string;
  readonly mime: string;
  readonly ignored: boolean;
  readonly denied: boolean;
}

export interface ContentPayload {
  readonly path: string;
  readonly mime: string;
  readonly size: number;
  readonly offset: number;
  readonly data: string;
  readonly encoding?: "base64" | undefined;
  readonly truncated: boolean;
}

export interface ChangedPayload {
  readonly path: string;
  readonly kind: string;
}

export interface GitStatusPayload {
  readonly branch: string;
  readonly detached: boolean;
  /** The primary remote's name, or empty when the worktree has none. */
  readonly remote: string;
  readonly upstream: boolean;
  readonly ahead: number;
  readonly behind: number;
  readonly dirty_file_count: number;
  readonly head_sha: string;
}

export interface GitCommittedPayload {
  readonly commit: string;
  readonly branch: string;
  readonly files: number;
  /** The denylist paths left unstaged, where the runner left any. */
  readonly excluded?: readonly string[] | undefined;
}

export interface GitPushedPayload {
  readonly branch: string;
  readonly remote: string;
  readonly commit: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

/**
 * Frame assembly and chunk reassembly, with no socket and no state beyond the
 * chunk groups in flight. Split out for the same reason `SseParser` is: the
 * awkward part of a wire format is reassembly across boundaries, and it should
 * be testable by calling a method with a string.
 *
 * §18.2 says a payload over 256 KB arrives as `chunk {part, parts, data}`
 * frames "the receiver reassembles", and stops there: it does not say what
 * type the reassembled frame has. Two rules cover it, in this order. If the
 * chunk payload names one (`type`), that wins. Otherwise the type is inferred
 * from the reassembled payload's own shape, which is unambiguous across the
 * files channel's four answers.
 */
export class RelayFrameAssembler {
  private readonly groups = new Map<string, { parts: number; received: Map<number, string> }>();

  /** Parses one text message. Returns nothing while a chunk group is partial. */
  push(text: string): RelayFrame[] {
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch {
      // A frame this client cannot parse is dropped, never fatal: the same
      // rule `runtime/rpc/session.ts` applies to an undecodable event.
      return [];
    }
    if (!isRecord(parsed)) return [];
    const channel = typeof parsed.channel === "string" ? parsed.channel : "";
    const type = typeof parsed.type === "string" ? parsed.type : "";
    if (channel === "" || type === "") return [];
    const stream = typeof parsed.stream === "string" ? parsed.stream : undefined;
    const seq = typeof parsed.seq === "number" ? parsed.seq : undefined;
    if (type !== "chunk") {
      return [{ channel, type, ...(stream === undefined ? {} : { stream }), ...(seq === undefined ? {} : { seq }), payload: parsed.payload }];
    }
    return this.chunk(channel, stream, seq, parsed.payload);
  }

  /** Drops every partial group. Called when a stream is closed or restarted. */
  reset(): void {
    this.groups.clear();
  }

  private chunk(
    channel: string,
    stream: string | undefined,
    seq: number | undefined,
    payload: unknown,
  ): RelayFrame[] {
    if (!isRecord(payload)) return [];
    const part = typeof payload.part === "number" ? payload.part : 0;
    const parts = typeof payload.parts === "number" ? payload.parts : 0;
    const data = typeof payload.data === "string" ? payload.data : "";
    if (part < 1 || parts < 1 || part > parts) return [];
    const key = `${channel}:${stream ?? ""}`;
    const group = this.groups.get(key) ?? { parts, received: new Map<number, string>() };
    group.received.set(part, data);
    if (group.received.size < parts) {
      this.groups.set(key, group);
      return [];
    }
    this.groups.delete(key);
    // Parts are reassembled in index order, not arrival order: §18.2 numbers
    // them so a receiver does not have to trust the ordering of the socket.
    let joined = "";
    for (let index = 1; index <= parts; index += 1) joined += group.received.get(index) ?? "";
    let whole: unknown;
    try {
      whole = JSON.parse(joined);
    } catch {
      return [];
    }
    const named = typeof payload.type === "string" ? payload.type : null;
    return [
      {
        channel,
        type: named ?? inferChunkedType(whole, channel),
        ...(stream === undefined ? {} : { stream }),
        ...(seq === undefined ? {} : { seq }),
        payload: whole,
      },
    ];
  }
}

/**
 * Which answer a reassembled payload is, from the fields §18.4 and §18.12 give
 * it.
 *
 * The channel has to be asked, because the two channels disagree about one
 * field: a files `content` payload and an exec `output` payload both carry
 * `data: string`, so on `exec` the files rules below would call an `output`
 * frame a `content` one and a chunked span of a run's output would be
 * dropped. The parameter is optional so the existing call sites and the
 * existing test keep working, and `files` stays the default it is everywhere
 * else in this module.
 */
export function inferChunkedType(payload: unknown, channel?: string): string {
  if (channel === "terminal") {
    // A terminal's chunked payloads are output spans and errors, and nothing
    // else on the channel is big enough to be chunked: an `opened` is two
    // numbers and an `exit` is one. Without this branch a chunked span would
    // fall through to the files rules and be reassembled as `content`, which
    // is the same collision §18.12 had to name for `exec`.
    if (!isRecord(payload)) return "output";
    if (typeof payload.code === "string") return "error";
    return "output";
  }
  if (channel === "exec") {
    if (!isRecord(payload)) return "output";
    // `exited` carries a numeric `code`; an `error` carries a string one.
    if (typeof payload.code === "number") return "exited";
    if (typeof payload.code === "string") return "error";
    return "output";
  }
  if (!isRecord(payload)) return "content";
  if (Array.isArray(payload.entries)) return "listed";
  if (typeof payload.code === "string") return "error";
  if (typeof payload.data === "string") return "content";
  if (typeof payload.kind === "string" && typeof payload.mime === "string") return "stat";
  return "content";
}

// --- Transport --------------------------------------------------------------

export interface RelaySocket {
  send(data: string): void;
  close(): void;
}

export interface RelayHandlers {
  readonly onOpen: () => void;
  readonly onMessage: (data: string) => void;
  /** The socket ended. `detail` is for the log, not for the reader. */
  readonly onClose: (detail: string) => void;
}

/** Exactly the seam `fetchEventStreamTransport` is for SSE. */
export type RelayTransport = (url: string, handlers: RelayHandlers) => RelaySocket;

export const webSocketTransport: RelayTransport = (url, handlers) => {
  const socket = new WebSocket(url);
  let settled = false;
  const end = (detail: string) => {
    if (settled) return;
    settled = true;
    handlers.onClose(detail);
  };
  socket.addEventListener("open", () => handlers.onOpen());
  socket.addEventListener("message", (event: MessageEvent<unknown>) => {
    if (typeof event.data === "string") handlers.onMessage(event.data);
  });
  socket.addEventListener("error", () => end("The relay connection failed."));
  socket.addEventListener("close", (event: CloseEvent) =>
    end(event.reason.length > 0 ? event.reason : "The relay connection closed."),
  );
  return {
    send: (data) => {
      if (socket.readyState === WebSocket.OPEN) socket.send(data);
    },
    close: () => socket.close(),
  };
};

// --- The client -------------------------------------------------------------

export type RelayState = "idle" | "connecting" | "open" | "closed";

export interface WorkspaceRelayOptions {
  /** Same-origin relay path; the ticket is appended by `mintTicket`'s caller. */
  readonly url: (ticket: string) => string;
  /** Mints a single-use ticket. Called once per socket, reconnects included. */
  readonly mintTicket: () => Promise<string>;
  readonly transport?: RelayTransport;
  readonly channel?: RelayChannel;
  /** Overridable only so a test can assert the batching without wall clock. */
  readonly ackIntervalMs?: number;
  readonly ackThreshold?: number;
}

export interface ListOptions {
  readonly cursor?: string | undefined;
  readonly showIgnored?: boolean | undefined;
}

export interface ReadOptions {
  readonly offset?: number | undefined;
  readonly length?: number | undefined;
}

export interface WorkspaceRelay {
  list(path: string, options?: ListOptions): Promise<ListedPayload>;
  stat(path: string): Promise<StatPayload>;
  read(path: string, options?: ReadOptions): Promise<ContentPayload>;
  /**
   * Asks the runner to watch a path.
   *
   * Returns nothing on purpose. §18.4 answers a `watch` with `changed` frames
   * or with an `unsupported` error and names no acknowledgement, so there is
   * no event a promise could honestly settle on — a runner that supports
   * watching and sees no change is indistinguishable from one that is slow.
   * `onUnavailable` is the cue §18.4 does name: the client falls back to
   * refreshing on focus.
   */
  watch(
    path: string,
    onChanged: (event: ChangedPayload) => void,
    onUnavailable?: (error: RelayError) => void,
  ): void;

  // The git channel (§18.12). Three requests, three answers, no streaming
  // state of their own: each is one round trip on a stream allocated exactly
  // the way a files request allocates one.
  gitStatus(): Promise<GitStatusPayload>;
  gitCommit(message: string): Promise<GitCommittedPayload>;
  gitPush(): Promise<GitPushedPayload>;

  close(): void;
  readonly state: RelayState;
}

/** §18.2's ack cadence: whichever of the two comes first. */
const ACK_THRESHOLD = 32;
const ACK_INTERVAL_MS = 1_000;

interface Pending {
  readonly seq: number;
  /** Which answer type settles this request. */
  readonly answer:
    | "listed"
    | "stat"
    | "content"
    | "watched"
    | "status"
    | "committed"
    | "pushed";
  resolve(payload: unknown): void;
  reject(error: RelayError): void;
}

export function createWorkspaceRelay(options: WorkspaceRelayOptions): WorkspaceRelay {
  const channel: RelayChannel = options.channel ?? "files";
  const transport = options.transport ?? webSocketTransport;
  const ackThreshold = options.ackThreshold ?? ACK_THRESHOLD;
  const ackIntervalMs = options.ackIntervalMs ?? ACK_INTERVAL_MS;

  const assembler = new RelayFrameAssembler();
  let socket: RelaySocket | null = null;
  let state: RelayState = "idle";
  let disposed = false;

  /** The hub-allocated stream id, once the first answer has named one. */
  let streamId: string | null = null;
  /** True between sending the first request and learning the stream id. */
  let allocating = false;
  /** Outbound `seq`, per §18.2 per stream per direction, from 1. */
  let outboundSeq = 0;
  /** The highest inbound `seq` seen, which is what `resume` replays from. */
  let inboundSeq = 0;
  let unacked = 0;
  let ackTimer: ReturnType<typeof setTimeout> | undefined;

  const pending: Pending[] = [];
  /** Requests minted before the socket was open, or during stream allocation. */
  const queued: Array<() => void> = [];
  const watchers = new Set<(event: ChangedPayload) => void>();
  let resuming = false;

  function fail(error: RelayError): void {
    const settling = pending.splice(0, pending.length);
    for (const entry of settling) entry.reject(error);
  }

  function clearAckTimer(): void {
    if (ackTimer === undefined) return;
    clearTimeout(ackTimer);
    ackTimer = undefined;
  }

  function flushAck(): void {
    clearAckTimer();
    if (unacked === 0 || streamId === null || socket === null) return;
    unacked = 0;
    socket.send(
      JSON.stringify({ channel, stream: streamId, type: "ack", payload: { through: inboundSeq } }),
    );
  }

  function noteInbound(seq: number | undefined): void {
    if (seq === undefined) return;
    if (seq > inboundSeq) inboundSeq = seq;
    unacked += 1;
    if (unacked >= ackThreshold) {
      flushAck();
      return;
    }
    if (ackTimer === undefined) ackTimer = setTimeout(flushAck, ackIntervalMs);
  }

  /** Settles the oldest unanswered request this answer can belong to. */
  function settle(answer: Pending["answer"], payload: unknown): void {
    const index = pending.findIndex((entry) => entry.answer === answer);
    if (index < 0) return;
    const [entry] = pending.splice(index, 1);
    entry?.resolve(payload);
  }

  function settleError(payload: unknown): void {
    const record = isRecord(payload) ? payload : {};
    const code = typeof record.code === "string" ? record.code : "invalid_frame";
    const message = typeof record.message === "string" ? record.message : undefined;
    const seq = typeof record.seq === "number" ? record.seq : null;
    const stderr = typeof record.stderr === "string" ? record.stderr : null;
    const error = new RelayError(code, message, seq, stderr);
    if (code === "resume_failed") {
      // The hub has forgotten the stream. Everything in flight fails and the
      // queue is re-sent on a freshly allocated stream rather than lost.
      restart(error);
      drainQueue();
      return;
    }
    // The hub names the failing request's `seq` where it can (§18.4). Where it
    // cannot, the error falls to the oldest request that is actually waiting
    // for an answer — never to a `watch`, which has none to wait for.
    const index =
      seq === null
        ? Math.max(
            pending.findIndex((entry) => entry.answer !== "watched"),
            0,
          )
        : pending.findIndex((entry) => entry.seq === seq);
    if (index < 0 || pending.length === 0) return;
    const [entry] = pending.splice(index, 1);
    entry?.reject(error);
  }

  /**
   * The stream is gone for good. Everything waiting is failed and the client
   * goes back to having no stream, so the next request allocates a fresh one
   * rather than resuming a stream the hub has already forgotten (§18.2).
   */
  function restart(error: RelayError): void {
    streamId = null;
    allocating = false;
    outboundSeq = 0;
    inboundSeq = 0;
    unacked = 0;
    clearAckTimer();
    assembler.reset();
    resuming = false;
    fail(error);
  }

  function onFrame(frame: RelayFrame): void {
    if (frame.channel !== channel) return;
    // The first answer names the stream every later request reuses (§18.2).
    if (streamId === null && frame.stream !== undefined) {
      streamId = frame.stream;
      allocating = false;
      drainQueue();
    }
    noteInbound(frame.seq);
    switch (frame.type) {
      case "listed":
        settle("listed", frame.payload);
        return;
      case "stat":
        settle("stat", frame.payload);
        return;
      case "content":
        settle("content", frame.payload);
        return;
      case "status":
        settle("status", frame.payload);
        return;
      case "committed":
        settle("committed", frame.payload);
        return;
      case "pushed":
        settle("pushed", frame.payload);
        return;
      case "changed": {
        const payload = isRecord(frame.payload) ? frame.payload : {};
        const event: ChangedPayload = {
          path: typeof payload.path === "string" ? payload.path : "",
          kind: typeof payload.kind === "string" ? payload.kind : "",
        };
        for (const watcher of watchers) watcher(event);
        return;
      }
      case "watched":
        settle("watched", frame.payload);
        return;
      case "resumed":
        resuming = false;
        drainQueue();
        return;
      case "closed":
        restart(new RelayError("workspace_closed"));
        return;
      case "error":
        settleError(frame.payload);
        return;
      default:
        // §18.2: an unknown type is dropped rather than acted on.
        return;
    }
  }

  /**
   * Sends what is waiting, one at a time and re-checking between each.
   * Draining the whole queue in one pass would put a second request on the
   * wire before the first had been told which stream it belongs to, and the
   * hub would allocate two streams for what is one logical conversation.
   */
  function drainQueue(): void {
    while (queued.length > 0) {
      if (socket === null || state !== "open" || resuming || allocating) return;
      queued.shift()?.();
    }
  }

  function connect(): void {
    if (disposed || socket !== null || state === "connecting") return;
    state = "connecting";
    const resumeStream = streamId;
    const resumeFrom = inboundSeq;
    void options
      .mintTicket()
      .then((ticket) => {
        if (disposed) return;
        const opened = transport(options.url(ticket), {
          onOpen: () => {
            state = "open";
            if (resumeStream !== null) {
              // A reconnect resumes rather than re-opens: §18.2 gives 60
              // seconds and a replay buffer, and re-listing every open
              // directory would be both slower and visibly different.
              resuming = true;
              opened.send(
                JSON.stringify({
                  channel,
                  type: "resume",
                  payload: { stream: resumeStream, last_seq: resumeFrom },
                }),
              );
              return;
            }
            drainQueue();
          },
          onMessage: (data) => {
            for (const frame of assembler.push(data)) onFrame(frame);
          },
          onClose: (detail) => {
            socket = null;
            clearAckTimer();
            if (disposed) {
              state = "closed";
              return;
            }
            state = "idle";
            // Nothing reconnects on its own here: a socket is only opened by a
            // request, so the reader's next action pays for the reconnect and
            // an abandoned panel holds no socket open.
            if (pending.length > 0 || queued.length > 0) {
              fail(new RelayError("resume_failed", detail));
            }
          },
        });
        socket = opened;
      })
      .catch((cause: unknown) => {
        if (disposed) return;
        state = "idle";
        socket = null;
        fail(
          new RelayError(
            "resume_failed",
            cause instanceof Error ? cause.message : String(cause),
          ),
        );
      });
  }

  interface RequestOptions {
    /**
     * Settle the promise as soon as the frame is on the wire. For `watch`,
     * which §18.4 gives no acknowledgement to: there is nothing to wait for,
     * and a failure arrives later through `onError` instead.
     */
    readonly resolveOnSend?: boolean;
    readonly onError?: (error: RelayError) => void;
  }

  function request<A>(
    type: string,
    answer: Pending["answer"],
    payload: unknown,
    requestOptions: RequestOptions = {},
  ): Promise<A> {
    if (disposed) return Promise.reject(new RelayError("workspace_closed"));
    const resolveOnSend = requestOptions.resolveOnSend === true;
    return new Promise<A>((resolve, reject) => {
      const settleError = (error: RelayError) => {
        requestOptions.onError?.(error);
        if (!resolveOnSend) reject(error);
      };
      const send = () => {
        if (socket === null) {
          settleError(new RelayError("resume_failed", "The relay is not connected."));
          return;
        }
        outboundSeq += 1;
        const seq = outboundSeq;
        pending.push({
          seq,
          answer,
          resolve: (value) => resolve(value as A),
          reject: settleError,
        });
        socket.send(
          JSON.stringify({
            channel,
            // The very first request carries no `stream`: the hub allocates
            // one and names it on the answer (§18.2).
            ...(streamId === null ? {} : { stream: streamId }),
            type,
            seq,
            payload,
          }),
        );
        if (streamId === null) allocating = true;
        if (resolveOnSend) resolve(undefined as A);
      };
      // One request at a time until the stream id is known, and nothing at all
      // until the socket is open or resumed.
      if (socket === null || state !== "open" || allocating || resuming) {
        queued.push(send);
        connect();
        return;
      }
      send();
    });
  }

  return {
    list: (path, listOptions) =>
      request<ListedPayload>("list", "listed", {
        path,
        ...(listOptions?.cursor === undefined ? {} : { cursor: listOptions.cursor }),
        ...(listOptions?.showIgnored === undefined
          ? {}
          : { show_ignored: listOptions.showIgnored }),
      }),
    stat: (path) => request<StatPayload>("stat", "stat", { path }),
    read: (path, readOptions) =>
      request<ContentPayload>("read", "content", {
        path,
        ...(readOptions?.offset === undefined ? {} : { offset: readOptions.offset }),
        ...(readOptions?.length === undefined ? {} : { length: readOptions.length }),
      }),
    gitStatus: () => request<GitStatusPayload>("status", "status", {}),
    gitCommit: (message) => request<GitCommittedPayload>("commit", "committed", { message }),
    // No payload: §18.12's push takes none. The branch and its remote are the
    // worktree's own, and naming them here would let a client push somewhere
    // the status it just read does not describe.
    gitPush: () => request<GitPushedPayload>("push", "pushed", {}),
    watch: (path, onChanged, onUnavailable) => {
      watchers.add(onChanged);
      void request<void>("watch", "watched", { path }, {
        resolveOnSend: true,
        onError: (error) => {
          // §18.4: an `unsupported` answer is the cue to stop expecting
          // `changed` frames and refresh on focus instead.
          watchers.delete(onChanged);
          onUnavailable?.(error);
        },
      });
    },
    close: () => {
      disposed = true;
      clearAckTimer();
      if (socket !== null && streamId !== null) {
        socket.send(JSON.stringify({ channel, stream: streamId, type: "close" }));
      }
      socket?.close();
      socket = null;
      state = "closed";
      fail(new RelayError("workspace_closed"));
    },
    get state() {
      return state;
    },
  };
}

/** Decodes a `content` payload to text, honouring §18.2's base64 encoding. */
export function decodeContent(payload: ContentPayload): string {
  if (payload.encoding !== "base64") return payload.data;
  try {
    const binary = globalThis.atob(payload.data);
    const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch {
    return "";
  }
}
