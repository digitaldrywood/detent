// One PTY over the relay's `terminal` channel (decisions.md §18.3).
//
// **Why this is a third client beside `workspaceRelay.ts` and `actionRuns.ts`.**
// `workspaceRelay.ts` is request/response: one request settles one promise, and
// a socket is only opened because a reader asked for something, so nothing
// reconnects on its own. `actionRuns.ts` is one-shot streaming: one `run`, an
// unbounded number of `output` frames, then `exited`, then `closed`, and a
// dropped socket is a failed run because the runner kills the process group
// when the socket dies.
//
// A terminal is neither. It streams in both directions for as long as the
// person is looking at it, and it is the one thing on this relay that survives
// its socket: §18.2 gives a dropped connection sixty seconds to come back, the
// runner keeps a disconnected PTY alive exactly that long, and a `resume
// {stream, last_seq}` on a fresh ticket puts the person back in front of the
// same shell with the output they missed. So this client reconnects on its own
// — which neither of the others may do — and it is the reconnect that makes it
// its own file rather than a mode inside one of them.
//
// What is shared is shared as code, the way §18.12's client shares it:
// `RelayFrameAssembler` frames and reassembles, `RelayError` and
// `relayErrorMessage` name the failures, `webSocketTransport` is the same
// transport seam, and §18.2's acknowledgement cadence is the same 32 frames or
// one second.
//
// One rule of §18.2 is load-bearing here and invisible everywhere else:
// person-originated frames are never replayed. A keystroke whose delivery
// cannot be established is reported as `unknown_delivery` and dropped, so this
// client never re-sends input across a reconnect. Re-sending it would be worse
// than losing it: the shell may already have run it.
import React from "react";

import {
  RelayError,
  RelayFrameAssembler,
  webSocketTransport,
  type RelayFrame,
  type RelaySocket,
  type RelayTransport,
} from "./workspaceRelay.ts";

const CHANNEL = "terminal";

/** §18.2's ack cadence, the same two numbers every other client uses. */
const ACK_THRESHOLD = 32;
const ACK_INTERVAL_MS = 1_000;

/**
 * How long to wait before redialling a dropped socket.
 *
 * It is well inside §18.2's sixty-second resume window, so a reconnect that
 * works lands with time to spare, and long enough that a hub refusing
 * connections is not hammered by every open tab at once.
 */
const RECONNECT_DELAY_MS = 750;

/** §18.3's own window defaults, for a client that has not measured yet. */
export const DEFAULT_COLS = 80;
export const DEFAULT_ROWS = 24;

/**
 * What the Terminal surface draws.
 *
 * `status` is the client's own vocabulary rather than the hub's, because the
 * hub has none for a terminal: a PTY is not a row anywhere. `reconnecting` is
 * the state §18.2's resume window exists for, and it is deliberately separate
 * from `connecting` — a reader who has a shell and lost the connection is in a
 * different situation from one who never had a shell.
 */
export type TerminalStatus =
  | "connecting"
  | "open"
  | "reconnecting"
  | "exited"
  | "failed";

export interface TerminalSnapshot {
  readonly status: TerminalStatus;
  /** The shell's process id, once the runner has answered `opened`. */
  readonly pid: number | null;
  /** The level the PTY actually runs at, which the runner reports. */
  readonly isolation: string | null;
  readonly exitCode: number | null;
  readonly signal: string | null;
  /** Why it failed, as a sentence. Null while it is fine. */
  readonly error: string | null;
}

export interface TerminalStreamOptions {
  /** Same-origin relay path; the ticket is what a browser can carry (§18.2). */
  readonly url: (ticket: string) => string;
  readonly mintTicket: () => Promise<string>;
  readonly transport?: RelayTransport;
  readonly ackIntervalMs?: number;
  readonly ackThreshold?: number;
  readonly reconnectDelayMs?: number;
  readonly cols?: number;
  readonly rows?: number;
  /** Every span the shell wrote, decoded from §18.3's base64. */
  readonly onOutput: (data: string) => void;
  readonly onChange: (snapshot: TerminalSnapshot) => void;
}

export interface TerminalStream {
  /** What the person typed. Dropped when the socket is down (§18.2). */
  write(data: string): void;
  /** The window changed. The runner turns it into a real winsize (§18.3). */
  resize(cols: number, rows: number): void;
  close(): void;
  readonly snapshot: TerminalSnapshot;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

/**
 * Decodes one `output` payload.
 *
 * §18.3 makes a terminal's output always base64, unlike the exec channel's: a
 * PTY's bytes are text interleaved with escape sequences, and a span cut at a
 * frame boundary routinely ends inside one. A span that arrives as plain text
 * is still honoured — a hub that stopped encoding would not be a reason to show
 * a reader nothing.
 */
export function decodeTerminalOutput(payload: unknown): string | null {
  if (!isRecord(payload)) return null;
  const data = typeof payload.data === "string" ? payload.data : null;
  if (data === null) return null;
  if (payload.encoding !== "base64") return data;
  try {
    const binary = globalThis.atob(data);
    const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    // `stream: true` keeps a multi-byte rune split across two frames together:
    // a PTY chunks on bytes and knows nothing about runes.
    return terminalDecoder.decode(bytes, { stream: true });
  } catch {
    return null;
  }
}

/**
 * One decoder per module rather than per span.
 *
 * A decoder with `stream: true` carries the tail of a split rune to the next
 * call, which is the whole point of using one; constructing a fresh one per
 * span would throw that tail away and render two replacement glyphs where the
 * person typed one character.
 */
const terminalDecoder = new TextDecoder();

export function createTerminalStream(options: TerminalStreamOptions): TerminalStream {
  const transport = options.transport ?? webSocketTransport;
  const ackThreshold = options.ackThreshold ?? ACK_THRESHOLD;
  const ackIntervalMs = options.ackIntervalMs ?? ACK_INTERVAL_MS;
  const reconnectDelayMs = options.reconnectDelayMs ?? RECONNECT_DELAY_MS;

  let snapshot: TerminalSnapshot = {
    status: "connecting",
    pid: null,
    isolation: null,
    exitCode: null,
    signal: null,
    error: null,
  };

  let assembler = new RelayFrameAssembler();
  let socket: RelaySocket | null = null;
  let streamId: string | null = null;
  let cols = options.cols ?? DEFAULT_COLS;
  let rows = options.rows ?? DEFAULT_ROWS;
  let disposed = false;
  let settled = false;
  let resuming = false;
  let inboundSeq = 0;
  let outboundSeq = 0;
  let unacked = 0;
  let ackTimer: ReturnType<typeof setTimeout> | undefined;
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined;

  function publish(next: Partial<TerminalSnapshot>): void {
    snapshot = { ...snapshot, ...next };
    options.onChange(snapshot);
  }

  function clearAckTimer(): void {
    if (ackTimer === undefined) return;
    clearTimeout(ackTimer);
    ackTimer = undefined;
  }

  function clearReconnectTimer(): void {
    if (reconnectTimer === undefined) return;
    clearTimeout(reconnectTimer);
    reconnectTimer = undefined;
  }

  function flushAck(): void {
    clearAckTimer();
    if (unacked === 0 || streamId === null || socket === null) return;
    unacked = 0;
    socket.send(
      JSON.stringify({
        channel: CHANNEL,
        stream: streamId,
        type: "ack",
        payload: { through: inboundSeq },
      }),
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

  /**
   * A terminal status, written once.
   *
   * A shell that exited and a shell whose connection failed are both endings,
   * and neither is something to reconnect out of: §18.2's window is for a
   * socket that dropped under a live PTY, not for a PTY that is gone.
   */
  function finish(next: Partial<TerminalSnapshot> & { status: "exited" | "failed" }): void {
    if (settled) return;
    settled = true;
    clearAckTimer();
    clearReconnectTimer();
    publish(next);
  }

  /** The next person-bound sequence. §18.2: per stream, per direction, from 1. */
  function nextSeq(): number {
    outboundSeq += 1;
    return outboundSeq;
  }

  function send(type: string, payload?: unknown): void {
    if (socket === null || streamId === null) return;
    socket.send(JSON.stringify({ channel: CHANNEL, stream: streamId, type, seq: nextSeq(), payload }));
  }

  function onFrame(frame: RelayFrame): void {
    if (frame.channel !== CHANNEL) return;
    // The first answer names the stream every later frame of this PTY uses
    // (§18.2). A resume answers with the same id, so this is written once.
    if (streamId === null && frame.stream !== undefined) streamId = frame.stream;
    noteInbound(frame.seq);
    switch (frame.type) {
      case "opened": {
        const payload = isRecord(frame.payload) ? frame.payload : {};
        publish({
          status: "open",
          pid: typeof payload.pid === "number" ? payload.pid : null,
          isolation: typeof payload.isolation === "string" ? payload.isolation : null,
          error: null,
        });
        return;
      }
      case "output": {
        const data = decodeTerminalOutput(frame.payload);
        if (data === null || data.length === 0) return;
        options.onOutput(data);
        return;
      }
      case "resumed":
        // The stream is ours again and the replay follows. The status goes back
        // to open here rather than on the next output span, because a shell
        // that is idle would otherwise leave the reader looking at
        // "reconnecting" for as long as they did not type.
        resuming = false;
        publish({ status: "open", error: null });
        return;
      case "exit": {
        const payload = isRecord(frame.payload) ? frame.payload : {};
        const code = typeof payload.code === "number" ? payload.code : null;
        const signal = typeof payload.signal === "string" ? payload.signal : null;
        finish({ status: "exited", exitCode: code, signal, error: null });
        return;
      }
      case "closed":
        // The runner follows `exit` with `closed` so the hub gets the stream's
        // slot back, so this is normally a no-op. When it is not, the stream
        // ended under a shell that never reported an exit.
        finish({
          status: "exited",
          exitCode: null,
          signal: null,
          error: "The terminal's stream closed before the shell exited.",
        });
        return;
      case "error": {
        const payload = isRecord(frame.payload) ? frame.payload : {};
        const code = typeof payload.code === "string" ? payload.code : "invalid_frame";
        const message = typeof payload.message === "string" ? payload.message : undefined;
        if (code === "unknown_delivery") {
          // §18.2: a person-originated frame whose delivery could not be
          // established is dropped rather than replayed. The shell may already
          // have run it, so re-sending would be worse than losing it; the
          // reader sees nothing appear, which is the honest outcome.
          return;
        }
        if (code === "resume_failed") {
          // The window ran out, or the stream is gone. There is no shell to go
          // back to, so this is an ending rather than a retry.
          finish({
            status: "failed",
            error: "This terminal could not be resumed; open a new one.",
          });
          return;
        }
        finish({ status: "failed", error: new RelayError(code, message).message });
        return;
      }
      default:
        // §18.2: an unknown type is dropped rather than acted on.
        return;
    }
  }

  function connect(): void {
    if (disposed || settled) return;
    const resumeStream = streamId;
    const resumeFrom = inboundSeq;
    assembler = new RelayFrameAssembler();
    void options
      .mintTicket()
      .then((ticket) => {
        if (disposed || settled) return;
        const opened = transport(options.url(ticket), {
          onOpen: () => {
            if (resumeStream === null) {
              // The very first frame carries no `stream`: the hub allocates
              // one and names it on the answer (§18.2). Every `open`
              // allocates, because every open is its own PTY.
              opened.send(
                JSON.stringify({
                  channel: CHANNEL,
                  type: "open",
                  seq: nextSeq(),
                  payload: { cols, rows },
                }),
              );
              return;
            }
            // A reconnect is a new ticket, a new socket and a `resume` on it.
            // The sequence counter is not reset: it is per stream per
            // direction, and the stream is the same one.
            resuming = true;
            publish({ status: "reconnecting" });
            opened.send(
              JSON.stringify({
                channel: CHANNEL,
                type: "resume",
                payload: { stream: resumeStream, last_seq: resumeFrom },
              }),
            );
          },
          onMessage: (data) => {
            for (const frame of assembler.push(data)) onFrame(frame);
          },
          onClose: (detail) => {
            socket = null;
            clearAckTimer();
            if (disposed || settled) return;
            if (streamId === null) {
              // The socket died before a stream existed, so there is nothing
              // to resume and nothing the reader can do but open a new one.
              finish({ status: "failed", error: detail });
              return;
            }
            // A PTY survives its socket for sixty seconds (§18.2), so the
            // reader is told it is reconnecting rather than that their shell
            // is gone.
            publish({ status: "reconnecting", error: null });
            clearReconnectTimer();
            reconnectTimer = setTimeout(connect, reconnectDelayMs);
          },
        });
        socket = opened;
      })
      .catch((cause: unknown) => {
        if (disposed || settled) return;
        const detail = cause instanceof Error ? cause.message : String(cause);
        if (streamId === null) {
          finish({ status: "failed", error: detail });
          return;
        }
        // A ticket that could not be minted is a reason to try again inside
        // the window, not a reason to give the shell up.
        publish({ status: "reconnecting", error: null });
        clearReconnectTimer();
        reconnectTimer = setTimeout(connect, reconnectDelayMs);
      });
  }

  connect();

  return {
    write: (data) => {
      if (data.length === 0 || resuming) return;
      send("input", { data });
    },
    resize: (nextCols, nextRows) => {
      if (nextCols <= 0 || nextRows <= 0) return;
      if (nextCols === cols && nextRows === rows) return;
      cols = nextCols;
      rows = nextRows;
      // The size is remembered even when there is no socket, so a window
      // resized while the connection is down is the size the next `open` asks
      // for rather than a change nobody heard.
      send("resize", { cols, rows });
    },
    close: () => {
      disposed = true;
      clearAckTimer();
      clearReconnectTimer();
      if (socket !== null && streamId !== null) {
        socket.send(JSON.stringify({ channel: CHANNEL, stream: streamId, type: "close" }));
      }
      socket?.close();
      socket = null;
      finish({
        status: "exited",
        exitCode: null,
        signal: null,
        error: null,
      });
    },
    get snapshot() {
      return snapshot;
    },
  };
}

// --- The surface's view of a workspace's terminals ---------------------------

export interface TerminalHandle {
  readonly id: string;
  readonly snapshot: TerminalSnapshot;
  readonly stream: TerminalStream | null;
}

export interface TerminalsHandle {
  readonly terminals: readonly TerminalHandle[];
  readonly activeId: string | null;
  readonly select: (id: string) => void;
  /** Opens another shell. §18.2: a second one is its own stream and its own PTY. */
  readonly open: () => string;
  readonly closeTerminal: (id: string) => void;
  /**
   * Points one terminal's output at whatever is drawing it.
   *
   * The sink is registered rather than carried on the snapshot because what
   * consumes a span is an xterm instance only the view has, and because a span
   * must not be a state update: a shell printing a build log would otherwise
   * re-render the whole panel thousands of times. It answers the function that
   * unregisters it, so a view can hand it straight to an effect.
   */
  readonly registerOutput: (id: string, sink: (data: string) => void) => () => void;
}

export interface UseTerminalsInput {
  readonly workspaceId: string | null;
  readonly workspaceLive: boolean;
  readonly relayUrl: ((ticket: string) => string) | null;
  readonly mintTicket: (() => Promise<string>) | null;
  /** False while the surface is closed, so an unopened tab spends no PTY. */
  readonly enabled: boolean;
}

let terminalCounter = 0;

function nextTerminalId(): string {
  terminalCounter += 1;
  return `terminal-${terminalCounter}`;
}

/**
 * Holds the open terminals for one workspace.
 *
 * It opens the first one the moment the surface is enabled and the workspace is
 * live, and it closes every one of them when the workspace goes away: a PTY
 * whose workspace has ended is a shell nobody can reach, and §18.3 kills it on
 * the runner regardless.
 */
export function useTerminals(input: UseTerminalsInput): TerminalsHandle {
  const { workspaceId, workspaceLive, relayUrl, mintTicket, enabled } = input;
  const [terminals, setTerminals] = React.useState<readonly TerminalHandle[]>([]);
  const [activeId, setActiveId] = React.useState<string | null>(null);
  const streams = React.useRef(new Map<string, TerminalStream>());
  const outputs = React.useRef(new Map<string, (data: string) => void>());

  const publish = React.useCallback((id: string, snapshot: TerminalSnapshot) => {
    setTerminals((current) =>
      current.map((terminal) =>
        terminal.id === id ? { ...terminal, snapshot } : terminal,
      ),
    );
  }, []);

  const start = React.useCallback(
    (id: string) => {
      if (relayUrl === null || mintTicket === null) return;
      const stream = createTerminalStream({
        url: relayUrl,
        mintTicket,
        onOutput: (data) => outputs.current.get(id)?.(data),
        onChange: (snapshot) => publish(id, snapshot),
      });
      streams.current.set(id, stream);
      setTerminals((current) =>
        current.map((terminal) => (terminal.id === id ? { ...terminal, stream } : terminal)),
      );
    },
    [mintTicket, publish, relayUrl],
  );

  const open = React.useCallback((): string => {
    const id = nextTerminalId();
    setTerminals((current) => [
      ...current,
      {
        id,
        snapshot: {
          status: "connecting",
          pid: null,
          isolation: null,
          exitCode: null,
          signal: null,
          error: null,
        },
        stream: null,
      },
    ]);
    setActiveId(id);
    start(id);
    return id;
  }, [start]);

  const closeTerminal = React.useCallback((id: string) => {
    streams.current.get(id)?.close();
    streams.current.delete(id);
    outputs.current.delete(id);
    setTerminals((current) => current.filter((terminal) => terminal.id !== id));
    setActiveId((current) => (current === id ? null : current));
  }, []);

  // The first terminal opens when the surface does, and not before: a panel
  // merely able to show one must not spend a PTY on the runner.
  React.useEffect(() => {
    if (!enabled || !workspaceLive || workspaceId === null) return;
    if (relayUrl === null || mintTicket === null) return;
    if (terminals.length > 0) return;
    open();
  }, [enabled, mintTicket, open, relayUrl, terminals.length, workspaceId, workspaceLive]);

  // A workspace that ended takes every shell with it.
  React.useEffect(() => {
    if (workspaceLive && enabled) return;
    const held = streams.current;
    if (held.size === 0) return;
    for (const stream of held.values()) stream.close();
    held.clear();
    outputs.current.clear();
    setTerminals([]);
    setActiveId(null);
  }, [enabled, workspaceLive]);

  // And so does the tab closing.
  React.useEffect(() => {
    const held = streams.current;
    return () => {
      for (const stream of held.values()) stream.close();
      held.clear();
    };
  }, []);

  React.useEffect(() => {
    if (activeId !== null || terminals.length === 0) return;
    setActiveId(terminals[terminals.length - 1]?.id ?? null);
  }, [activeId, terminals]);

  const registerOutput = React.useCallback((id: string, sink: (data: string) => void) => {
    outputs.current.set(id, sink);
    return () => {
      if (outputs.current.get(id) === sink) outputs.current.delete(id);
    };
  }, []);

  return { terminals, activeId, select: setActiveId, open, closeTerminal, registerOutput };
}
